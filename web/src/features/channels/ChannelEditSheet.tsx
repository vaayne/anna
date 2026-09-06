import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Sheet, SheetPopup } from "@/components/ui/sheet";
import { ChannelFields, channelConfig, type NormalizedChannel } from "./ChannelFields";
import { updateChannel } from "@/lib/api-client/sdk.gen";
import { apiErrorMessage } from "@/lib/api-error";
import { useI18n } from "@/lib/i18n";
import { FeishuPermissionSync } from "./FeishuPermissionSync";

type Notify = (message: string, kind?: "success" | "error") => void;

/**
 * Edit one existing channel in place, wherever it is listed. Creation stays on
 * `/settings/channels` (Feishu and WeChat register through a scan wizard) and so
 * does deletion, so this sheet only ever writes name, active state and credentials —
 * never `agent_id`, which the dedicated bind endpoint owns.
 *
 * `formKey` remounts the draft: the caller bumps it every time the sheet opens,
 * so a cancelled edit never leaks its fields into the next one.
 */
export function ChannelEditSheet({
  open,
  channel,
  formKey,
  onOpenChange,
  notify,
}: {
  open: boolean;
  channel: NormalizedChannel | null;
  formKey: number;
  onOpenChange: (open: boolean) => void;
  notify: Notify;
}) {
  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetPopup
        side="right"
        showCloseButton={false}
        className="w-full sm:w-[560px] sm:max-w-[560px]"
      >
        {channel && (
          <ChannelForm
            key={formKey}
            channel={channel}
            notify={notify}
            onDone={() => onOpenChange(false)}
          />
        )}
      </SheetPopup>
    </Sheet>
  );
}

function ChannelForm({
  channel: initial,
  notify,
  onDone,
}: {
  channel: NormalizedChannel;
  notify: Notify;
  onDone: () => void;
}) {
  const { t } = useI18n();
  const queryClient = useQueryClient();
  const [draft, setDraft] = useState<NormalizedChannel>(initial);
  const [overlayRoot, setOverlayRoot] = useState<HTMLDivElement | null>(null);

  const save = useMutation({
    mutationFn: () =>
      updateChannel({
        path: { id: draft.id },
        // Same body as the settings page minus `agent_id` and `type`: both are
        // tri-state on the backend, so omitting them keeps the stored value and
        // this sheet can never stomp a binding made from the row next to it.
        // `config` re-serializes the flat draft through `channelConfig`, which
        // keeps only the platform's declared keys — identical to a save from
        // /settings/channels.
        body: {
          name: draft.name || "",
          is_active: draft.is_active,
          config: channelConfig(draft),
        },
        throwOnError: true,
      }),
    onSuccess: async () => {
      notify(t("channels.saved"), "success");
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["channels"] }),
        queryClient.invalidateQueries({ queryKey: ["public-channels"] }),
      ]);
      onDone();
    },
    onError: (error) => notify(apiErrorMessage(error, t("channels.saveFailed")), "error"),
  });

  return (
    <div ref={setOverlayRoot} className="relative flex h-full min-h-0 flex-col">
      <div className="flex items-center gap-3 border-b p-5">
        <h2 className="min-w-0 flex-1 truncate text-base font-semibold">
          {t("channels.editTitle")}
        </h2>
        <Button size="icon-sm" variant="ghost" aria-label={t("common.close")} onClick={onDone}>
          <X size={16} />
        </Button>
      </div>

      <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-5">
        <p className="font-mono text-xs text-muted-foreground">{draft.id}</p>
        <ChannelFields
          channel={draft}
          onChange={(key, value) => setDraft((prev) => ({ ...prev, [key]: value }))}
        />
        <FeishuPermissionSync
          channel={draft}
          overlayRoot={overlayRoot}
          onAligned={async (channel) => {
            setDraft(channel);
            notify(t("channels.scanAlignFeishuDone"), "success");
            await Promise.all([
              queryClient.invalidateQueries({ queryKey: ["channels"] }),
              queryClient.invalidateQueries({ queryKey: ["public-channels"] }),
            ]);
          }}
        />
      </div>

      <div className="flex shrink-0 items-center justify-end gap-2 border-t p-4">
        <Button variant="ghost" disabled={save.isPending} onClick={onDone}>
          {t("common.cancel")}
        </Button>
        <Button loading={save.isPending} onClick={() => save.mutate()}>
          {t("common.save")}
        </Button>
      </div>
    </div>
  );
}
