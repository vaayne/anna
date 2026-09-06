import { useCallback, useMemo, useState } from "react";
import { Link, useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  createChannel as createChannelRequest,
  deleteChannel,
  unlinkProfileIdentity,
  updateChannel,
} from "@/lib/api-client/sdk.gen";
import type { Channel, Identity } from "@/lib/types";
import { allAgentsAdminQueryOptions } from "@/lib/queries/agents";
import { channelsQueryOptions, profileIdentitiesQueryOptions } from "@/lib/queries/channels";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Spinner } from "@/components/ui/spinner";
import { ErrorState } from "@/components/RouteFallback";
import { useI18n } from "@/lib/i18n";
import { errorMessage } from "@/lib/utils";
import { useToast } from "@/hooks/use-toast";
import {
  DetailPanel,
  DetailPanelHeader,
  FormSectionTitle,
} from "@/features/settings/SettingsDetailPanel";
import {
  SettingsCard,
  SettingsCardSection,
  SettingsDetailSheet,
  SettingsGridPage,
} from "@/features/settings/SettingsCardGrid";
import { SettingsEmptyState } from "@/features/settings/SettingsEmptyState";
import { ConfirmDialog } from "@/features/settings/ConfirmDialog";
import { Plus } from "lucide-react";
import { PlatformIcon, platformLabel } from "@/components/PlatformIcon";
import {
  ChannelFields,
  channelConfig,
  channelString,
  defaultChannelType,
  hasConfig,
  normalizeChannel,
  type ChannelForm,
  type ChannelFormValue,
  type NormalizedChannel,
} from "./ChannelFields";
import { NewChannelForm, newChannelDraftError } from "./NewChannelForm";
import { FeishuPermissionSync } from "./FeishuPermissionSync";
import { bindableAgents } from "./channel-access";
import { useAccountLink, weixinQrStatusVariant } from "./use-account-link";

// ─── ChannelDetail ────────────────────────────────────────────────────────────

interface ChannelDetailProps {
  channel: NormalizedChannel;
  identity: Identity | null;
  generating: boolean;
  linkPlatform: string;
  linkCode: string;
  wxQrUrl: string;
  wxQrStatus: string;
  wxQrPolling: boolean;
  onSave: (ch: NormalizedChannel) => Promise<void>;
  saving: boolean;
  /**
   * Ask the page to confirm the delete. The confirmation is an overlay and this
   * detail renders inside a Sheet, so the page owns it — nesting overlays is a
   * bug (`web-ui.md`).
   */
  onRequestDelete: (ch: NormalizedChannel) => void;
  onGenerateCode: (platform: string) => void;
  onStartWeixinQR: () => void;
  onUnlink: (id: string | undefined) => void;
  onCopyLinkCode: () => void;
  wxQrStatusVariant: (status: string) => "warning" | "info" | "success" | "error" | "secondary";
  onRefreshWxQr: () => void;
  onFeishuAligned: (channel: NormalizedChannel) => Promise<void>;
}

function ChannelDetail({
  channel: initialChannel,
  identity,
  generating,
  linkPlatform,
  linkCode,
  wxQrUrl,
  wxQrStatus,
  wxQrPolling,
  onSave,
  saving,
  onRequestDelete,
  onGenerateCode,
  onStartWeixinQR,
  onUnlink,
  onCopyLinkCode,
  wxQrStatusVariant,
  onRefreshWxQr,
  onFeishuAligned,
}: ChannelDetailProps) {
  const { t } = useI18n();
  // The keyed detail owns an optional draft. Query refetches update the clean
  // view through `initialChannel`, while an in-progress draft stays untouched.
  const [draft, setDraft] = useState<NormalizedChannel | null>(null);
  const [overlayRoot, setOverlayRoot] = useState<HTMLDivElement | null>(null);
  const channel = draft ?? initialChannel;

  const updateField = (key: string, value: ChannelFormValue) => {
    setDraft((prev) => ({ ...(prev ?? initialChannel), [key]: value }));
  };

  const save = async () => {
    const submitted = channel;
    try {
      await onSave(submitted);
      // If the user kept typing while the request was in flight, retain that
      // newer draft instead of clearing it with the older successful response.
      setDraft((current) => (current === submitted ? null : current));
    } catch {
      // The mutation owns the error toast. Keep the draft available for retry.
    }
  };

  const handleFeishuAligned = async (aligned: NormalizedChannel) => {
    // Alignment is an explicit save. Adopt its complete response before the
    // cache refetch so a pre-existing draft cannot write the old config back.
    setDraft(aligned);
    await onFeishuAligned(aligned);
    setDraft((current) => (current === aligned ? null : current));
  };

  const isDefaultInstance = channel.id === channel.type;
  const label = platformLabel(channel.type);

  return (
    <div ref={setOverlayRoot} className="relative h-full min-h-0">
      <DetailPanel
        onSave={() => void save()}
        isSaving={saving}
        onDelete={!isDefaultInstance ? () => onRequestDelete(channel) : undefined}
        saveLabel={t("common.save")}
        deleteLabel={t("common.delete")}
      >
        <DetailPanelHeader
          title={channel.name || label}
          subtitle={<p className="text-xs font-mono text-muted-foreground">{channel.type}</p>}
        />

        <ChannelFields channel={channel} onChange={updateField} />
        {hasConfig(channel.type, channel) && (
          <p className="text-xs text-muted-foreground">{t("channels.configOnlyNote")}</p>
        )}
        <FeishuPermissionSync
          channel={channel}
          overlayRoot={overlayRoot}
          onAligned={handleFeishuAligned}
        />

        {/* Identity / account section. */}
        <div className="space-y-3">
          <FormSectionTitle>My account</FormSectionTitle>
          {identity ? (
            <div className="space-y-2">
              <p className="text-xs text-muted-foreground">Linked identity</p>
              <p className="font-mono text-sm">
                {identity.name ? identity.name + " · " : ""}
                {identity.external_id}
              </p>
              <Button
                onClick={() => onUnlink(identity.id)}
                variant="ghost"
                size="sm"
                className="text-destructive-foreground"
              >
                Unlink
              </Button>
            </div>
          ) : (
            <div className="space-y-2">
              <p className="text-sm text-muted-foreground">No account linked yet.</p>
              {channel.type !== "weixin" && (
                <Button
                  onClick={() => onGenerateCode(channel.type)}
                  disabled={generating}
                  loading={generating && linkPlatform === channel.type}
                  size="sm"
                >
                  Link {label}
                </Button>
              )}
              {channel.type === "weixin" && (
                <Button onClick={onStartWeixinQR} loading={wxQrPolling} size="sm">
                  Link Weixin
                </Button>
              )}
            </div>
          )}

          {/* Link code */}
          {linkCode && linkPlatform === channel.type && (
            <div className="rounded-lg border border-border bg-card p-4 space-y-2">
              <p className="text-sm font-medium">Send this command to Stella on {label}:</p>
              <div className="flex items-center gap-2 flex-wrap">
                <code className="font-mono text-lg font-semibold bg-muted text-foreground px-3 py-1 rounded select-all">
                  /link {linkCode}
                </code>
                <Button onClick={onCopyLinkCode} variant="ghost" size="xs">
                  copy
                </Button>
              </div>
              <p className="text-xs text-muted-foreground">Expires in 5 minutes.</p>
            </div>
          )}

          {/* Weixin QR */}
          {wxQrUrl && channel.type === "weixin" && (
            <div className="rounded-xl border border-border bg-muted p-6 flex flex-col items-center">
              <p className="text-sm font-medium mb-2">Scan with WeChat to link your account</p>
              <img src={wxQrUrl} alt="WeChat QR Code" className="w-48 h-48 border rounded" />
              <Badge size="sm" variant={wxQrStatusVariant(wxQrStatus)} className="mt-2">
                {wxQrStatus}
              </Badge>
              {wxQrStatus === "expired" && (
                <Button onClick={onRefreshWxQr} variant="outline" size="xs" className="mt-1">
                  Refresh
                </Button>
              )}
            </div>
          )}
        </div>
      </DetailPanel>
    </div>
  );
}

// ─── main page ────────────────────────────────────────────────────────────────

export function ChannelsPage() {
  const { t } = useI18n();
  const navigate = useNavigate();
  // SAFETY: useParams returns the route param object; channelId is optional by route.
  const params = useParams({ strict: false }) as { channelId?: string };
  const channelId = params.channelId;
  // Creation opened from an agent's profile already knows the agent.
  // SAFETY: useSearch returns the URL search object; agent is optional.
  const search = useSearch({ strict: false }) as { agent?: string };
  const initialAgentId = search.agent ?? "";

  // The delete confirmation lives here, not in the detail: the detail renders
  // inside the Sheet and an overlay may not nest inside another (`web-ui.md`).
  const [pendingDelete, setPendingDelete] = useState<NormalizedChannel | null>(null);

  const { showToast } = useToast();
  const queryClient = useQueryClient();

  const channelsQuery = useQuery(channelsQueryOptions);
  const identitiesQuery = useQuery(profileIdentitiesQueryOptions);
  // This page historically requested include_all=true. Keep that visibility
  // contract while moving the response into the shared query cache.
  const agentsQuery = useQuery(allAgentsAdminQueryOptions(true));
  const rawChannels = channelsQuery.data ?? [];
  const linkedIdentities = identitiesQuery.data ?? [];
  const agents = agentsQuery.data ?? [];
  const instances = useMemo(
    () =>
      rawChannels.map(normalizeChannel).sort((a, b) => {
        const aDefault = a.id === a.type;
        const bDefault = b.id === b.type;
        if (aDefault !== bDefault) return aDefault ? -1 : 1;
        if (a.type !== b.type) return a.type.localeCompare(b.type);
        return a.id.localeCompare(b.id);
      }),
    [rawChannels],
  );

  // ── derived state ──

  const isCreating = channelId === "new";

  const selectedChannel = useMemo(
    () =>
      channelId && channelId !== "new" ? instances.find((ch) => ch.id === channelId) : undefined,
    [channelId, instances],
  );

  // ── helpers ──

  const identityFor = useCallback(
    (platform: string): Identity | null =>
      linkedIdentities.find((i) => i.platform === platform) || null,
    [linkedIdentities],
  );

  // The picker offers only what the API would accept; a readable-but-foreign
  // agent would just fail the bind.
  const pickableAgents = useMemo(() => bindableAgents(agents), [agents]);

  // ── account linking ──

  const link = useAccountLink({
    notify: showToast,
    onLinked: () =>
      void queryClient.invalidateQueries({ queryKey: profileIdentitiesQueryOptions.queryKey }),
  });

  // ── identity management ──

  const unlinkMutation = useMutation({
    mutationFn: (id: string) =>
      unlinkProfileIdentity({
        path: { id },
        throwOnError: true,
      }),
    onSuccess: async () => {
      showToast("Identity unlinked");
      await queryClient.invalidateQueries({ queryKey: profileIdentitiesQueryOptions.queryKey });
    },
    onError: (error) => showToast(errorMessage(error), "error"),
  });

  const unlinkIdentity = (id: string | undefined) => {
    if (!id || !confirm("Unlink this identity?")) return;
    unlinkMutation.mutate(id);
  };

  // ── instance management ──

  const refreshChannels = () =>
    Promise.all([
      queryClient.invalidateQueries({ queryKey: channelsQueryOptions.queryKey }),
      queryClient.invalidateQueries({ queryKey: ["public-channels"] }),
    ]);

  const saveMutation = useMutation({
    mutationFn: async (ch: NormalizedChannel) => {
      const { data: saved } = await updateChannel({
        path: { id: ch.id },
        body: {
          name: ch.name || "",
          type: ch.type,
          agent_id: ch.agent_id || "",
          is_active: ch.is_active,
          config: channelConfig(ch),
        },
        throwOnError: true,
      });
      return { submitted: ch, saved };
    },
    onSuccess: async ({ submitted, saved }) => {
      queryClient.setQueryData<Channel[]>(channelsQueryOptions.queryKey, (current) =>
        current?.map((channel) => (channel.id === saved.id ? saved : channel)),
      );
      await refreshChannels();
      showToast(submitted.id + " saved");
    },
    onError: (error) => showToast(errorMessage(error), "error"),
  });

  const saveInstance = (ch: NormalizedChannel) =>
    saveMutation.mutateAsync(ch).then(() => undefined);

  const deleteMutation = useMutation({
    mutationFn: (id: string) => deleteChannel({ path: { id }, throwOnError: true }),
    onSuccess: async (_, id) => {
      await refreshChannels();
      void navigate({ to: "/settings/channels" });
      showToast(id + " deleted");
    },
    onError: (error) => showToast(errorMessage(error), "error"),
  });

  const doDeleteChannel = (id: string) => {
    const ch = instances.find((c) => c.id === id);
    if (ch && ch.id === ch.type) {
      showToast("Default platform channels cannot be deleted", "error");
      return;
    }
    deleteMutation.mutate(id);
  };

  const finishRegisteredChannel = async (channel: NormalizedChannel) => {
    await refreshChannels();
    void navigate({
      to: "/settings/channels/$channelId",
      params: { channelId: channel.id },
    });
    showToast(channel.id + " created");
  };

  const finishAlignedFeishuChannel = async (channel: NormalizedChannel) => {
    const saved = {
      id: channel.id,
      name: channel.name,
      type: channel.type,
      agent_id: channel.agent_id,
      is_active: channel.is_active,
      config: channelConfig(channel),
    } satisfies Channel;
    queryClient.setQueryData<Channel[]>(channelsQueryOptions.queryKey, (current) =>
      current?.map((item) => (item.id === saved.id ? saved : item)),
    );
    await refreshChannels();
    showToast(t("channels.scanAlignFeishuDone"));
  };

  const createMutation = useMutation({
    mutationFn: async (draft: ChannelForm) => {
      // SAFETY: draft is a Record<string,unknown> channel draft; these fields carry
      // the string scalar values the create body requires.
      const { data: saved } = await createChannelRequest({
        // No id: the server mints an independent instance id for every platform.
        body: {
          name: channelString(draft.name),
          type: channelString(draft.type),
          agent_id: channelString(draft.agent_id),
          config: channelConfig(draft),
        },
        throwOnError: true,
      });
      return saved;
    },
    onSuccess: async (saved) => {
      await refreshChannels();
      void navigate({
        to: "/settings/channels/$channelId",
        params: { channelId: saved.id },
      });
      showToast(saved.id + " created");
    },
    onError: (error) => showToast(errorMessage(error), "error"),
  });

  const createNewChannel = async (draft: ChannelForm) => {
    const invalid = newChannelDraftError(draft, t);
    if (invalid) {
      showToast(invalid, "error");
      return;
    }
    await createMutation.mutateAsync(draft).catch(() => undefined);
  };

  // ── render ──

  // ── build detail pane ──

  let detail: React.ReactNode = undefined;

  if (isCreating) {
    detail = (
      <NewChannelForm
        fallbackChannelType={defaultChannelType}
        initialAgentId={initialAgentId}
        agents={pickableAgents}
        channels={instances}
        onAdd={createNewChannel}
        onRegistered={finishRegisteredChannel}
        onCancel={() => void navigate({ to: "/settings/channels" })}
        creating={createMutation.isPending}
      />
    );
  } else if (selectedChannel) {
    detail = (
      <ChannelDetail
        key={selectedChannel.id}
        channel={selectedChannel}
        identity={identityFor(selectedChannel.type)}
        generating={link.generating}
        linkPlatform={link.platform}
        linkCode={link.code}
        wxQrUrl={link.qrUrl}
        wxQrStatus={link.qrStatus}
        wxQrPolling={link.qrPolling}
        onSave={saveInstance}
        saving={saveMutation.isPending}
        onRequestDelete={setPendingDelete}
        onGenerateCode={(platform) => void link.generateCode(platform)}
        onStartWeixinQR={() => void link.startQr()}
        onUnlink={unlinkIdentity}
        onCopyLinkCode={link.copyCode}
        wxQrStatusVariant={weixinQrStatusVariant}
        onRefreshWxQr={() => void link.startQr()}
        onFeishuAligned={finishAlignedFeishuChannel}
      />
    );
  }

  // ── build card grid ──

  const sheetOpen = isCreating || !!selectedChannel;
  const closeSheet = () => void navigate({ to: "/settings/channels" });

  // Manageable instances, grouped by platform (the instances list is already
  // sorted default-instance-first within each type).
  const channelGroups = Object.values(
    instances.reduce<Record<string, { type: string; label: string; items: NormalizedChannel[] }>>(
      (acc, ch) => {
        (acc[ch.type] ??= {
          type: ch.type,
          label: platformLabel(ch.type),
          items: [],
        }).items.push(ch);
        return acc;
      },
      {},
    ),
  ).sort((a, b) => a.label.localeCompare(b.label));

  return (
    <>
      <SettingsGridPage
        title={t("channels.title")}
        action={
          <Button
            render={<Link to="/settings/channels/$channelId" params={{ channelId: "new" }} />}
            variant="outline"
            size="sm"
          >
            <Plus className="size-4" />
            {t("channels.addChannel")}
          </Button>
        }
      >
        {channelsQuery.isPending ? (
          <div className="flex justify-center py-8">
            <Spinner className="size-4" />
          </div>
        ) : channelsQuery.isError ? (
          <ErrorState
            title={t("route.error.title")}
            description={t("route.loadFailed")}
            onRetry={() => void channelsQuery.refetch()}
          />
        ) : (
          <>
            {(identitiesQuery.isError || agentsQuery.isError) && (
              <ErrorState
                title={t("route.error.title")}
                description={t("route.loadFailed")}
                onRetry={() => {
                  void identitiesQuery.refetch();
                  void agentsQuery.refetch();
                }}
              />
            )}
            {channelGroups.length === 0 ? (
              <SettingsEmptyState
                message={t("channels.noChannels")}
                description={t("channels.noChannelsDesc")}
              />
            ) : (
              channelGroups.map((group) => (
                <SettingsCardSection
                  key={group.type}
                  icon={<PlatformIcon type={group.type} />}
                  title={group.label}
                  count={group.items.length}
                >
                  {group.items.map((ch) => {
                    const label = platformLabel(ch.type);
                    const isDefault = ch.id === ch.type;
                    return (
                      <SettingsCard
                        key={ch.id}
                        icon={<PlatformIcon type={ch.type} />}
                        title={ch.name || label}
                        badge={
                          isDefault ? (
                            <Badge variant="secondary" size="sm">
                              default
                            </Badge>
                          ) : undefined
                        }
                        active={channelId === ch.id}
                        to="/settings/channels/$channelId"
                        params={{ channelId: ch.id }}
                        footer={
                          <>
                            <span
                              className={`size-1.5 shrink-0 rounded-full ${
                                ch.is_active ? "bg-success" : "bg-muted-foreground"
                              }`}
                            />
                            <span className="font-mono text-xs text-muted-foreground">{ch.id}</span>
                          </>
                        }
                      />
                    );
                  })}
                </SettingsCardSection>
              ))
            )}
          </>
        )}
      </SettingsGridPage>

      <SettingsDetailSheet open={sheetOpen} onClose={closeSheet}>
        {detail}
      </SettingsDetailSheet>

      <ConfirmDialog
        open={!!pendingDelete}
        onOpenChange={(open) => !open && setPendingDelete(null)}
        title={t("channels.deleteChannel")}
        message={pendingDelete ? t("channels.deleteChannelMsg", { id: pendingDelete.id }) : ""}
        onConfirm={() => {
          if (pendingDelete) doDeleteChannel(pendingDelete.id);
          setPendingDelete(null);
        }}
      />
    </>
  );
}
