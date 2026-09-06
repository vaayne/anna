import { useMemo } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Sheet, SheetPopup } from "@/components/ui/sheet";
import { createChannel } from "@/lib/api-client/sdk.gen";
import { apiErrorMessage } from "@/lib/api-error";
import { agentsQueryOptions } from "@/lib/queries/agents";
import { channelsQueryOptions } from "@/lib/queries/channels";
import { useI18n } from "@/lib/i18n";
import {
  channelConfig,
  channelString,
  defaultChannelType,
  normalizeChannel,
  type NormalizedChannel,
} from "./ChannelFields";
import { NewChannelForm, newChannelDraftError } from "./NewChannelForm";

type Notify = (message: string, kind?: "success" | "error") => void;

/**
 * Create a channel without leaving the agent that needs it. The binding is not
 * a question here — the entry point already answered it — so the form runs with
 * the agent locked and this sheet only owns the write and the cache.
 *
 * `formKey` remounts the draft: the caller bumps it every time the sheet opens,
 * so an abandoned draft never leaks into the next one.
 */
export function ChannelCreateSheet({
  open,
  agentId,
  formKey,
  onOpenChange,
  notify,
}: {
  open: boolean;
  agentId: string;
  formKey: number;
  onOpenChange: (open: boolean) => void;
  notify: Notify;
}) {
  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetPopup side="right" className="w-full sm:w-[560px] sm:max-w-[560px]">
        <CreateForm
          key={formKey}
          agentId={agentId}
          notify={notify}
          onDone={() => onOpenChange(false)}
        />
      </SheetPopup>
    </Sheet>
  );
}

function CreateForm({
  agentId,
  notify,
  onDone,
}: {
  agentId: string;
  notify: Notify;
  onDone: () => void;
}) {
  const { t } = useI18n();
  const queryClient = useQueryClient();
  const { data: agents = [] } = useQuery(agentsQueryOptions);
  const { data: rawChannels = [] } = useQuery(channelsQueryOptions);

  // The form reads channels only to hide agents that already own one on the
  // selected platform, and it speaks the flattened shape.
  const channels = useMemo<NormalizedChannel[]>(
    () => rawChannels.map(normalizeChannel),
    [rawChannels],
  );

  const refresh = () =>
    Promise.all([
      queryClient.invalidateQueries({ queryKey: ["channels"] }),
      queryClient.invalidateQueries({ queryKey: ["public-channels"] }),
    ]);

  const create = useMutation({
    mutationFn: (draft: import("./ChannelFields").ChannelForm) => {
      // SAFETY: draft is a Record<string,unknown> channel draft; these fields
      // carry the string scalar values the create body requires.
      const name = channelString(draft.name);
      // SAFETY: draft.type carries the platform discriminant as a string.
      const type = channelString(draft.type);
      // SAFETY: draft.agent_id is the string id when a bound agent is set.
      const agentId = channelString(draft.agent_id);
      return createChannel({
        body: {
          // No id: the server mints an independent instance id for every platform.
          name,
          type,
          agent_id: agentId,
          config: channelConfig(draft),
        },
        throwOnError: true,
      });
    },
    onSuccess: async () => {
      notify(t("channels.created"), "success");
      await refresh();
      onDone();
    },
    onError: (error) => notify(apiErrorMessage(error, t("channels.createFailed")), "error"),
  });

  return (
    <NewChannelForm
      fallbackChannelType={defaultChannelType}
      initialAgentId={agentId}
      lockAgent
      agents={agents}
      channels={channels}
      creating={create.isPending}
      onAdd={async (draft) => {
        const error = newChannelDraftError(draft, t);
        if (error) {
          notify(error, "error");
          return;
        }
        create.mutate(draft);
      }}
      onRegistered={async () => {
        notify(t("channels.created"), "success");
        await refresh();
        onDone();
      }}
      onCancel={onDone}
    />
  );
}
