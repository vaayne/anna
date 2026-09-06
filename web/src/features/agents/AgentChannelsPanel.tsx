import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Pencil, Plus } from "lucide-react";
import {
  AlertDialog,
  AlertDialogClose,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogPopup,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { PlatformIcon, platformLabel } from "@/components/PlatformIcon";
import { bindChannelAgent, unlinkProfileIdentity } from "@/lib/api-client/sdk.gen";
import { apiErrorMessage } from "@/lib/api-error";
import {
  channelsQueryOptions,
  profileIdentitiesQueryOptions,
  publicChannelsQueryOptions,
} from "@/lib/queries/channels";
import { meQueryOptions } from "@/lib/queries/me";
import type { Identity } from "@/lib/types";
import type { MessageKey } from "@/lib/i18n/messages";
import { useToast } from "@/hooks/use-toast";
import { useI18n } from "@/lib/i18n";
import { ChannelCreateSheet } from "@/features/channels/ChannelCreateSheet";
import { ChannelEditSheet } from "@/features/channels/ChannelEditSheet";
import { normalizeChannel, type NormalizedChannel } from "@/features/channels/ChannelFields";
import {
  LINK_CODE_PLATFORMS,
  QR_PLATFORM,
  useAccountLink,
  weixinQrStatusVariant,
} from "@/features/channels/use-account-link";
import { ProfilePanelSection, ProfileSectionMessage } from "./ProfilePanelSection";

const QR_STATUS_KEY = {
  waiting: "channels.qrWaiting",
  scaned: "channels.qrScanned",
  confirmed: "channels.qrConfirmed",
  expired: "channels.qrExpired",
} as const;

// SAFETY: qrStatus is a QR status key; the ?? fallback covers unknown values.
function qrStatusLabel(status: string): MessageKey {
  // SAFETY: status is a QR status key; the ?? fallback covers unknown values.
  return QR_STATUS_KEY[status as keyof typeof QR_STATUS_KEY] ?? "channels.qrWaiting";
}

/** One channel as this tab needs it, from either the admin or the public list. */
interface ChannelRow {
  id: string;
  type: string;
  name: string;
  agentId: string;
  is_active: boolean;
}

interface PlatformGroup {
  type: string;
  label: string;
  rows: ChannelRow[];
}

interface Props {
  agentId: string;
}

/**
 * Channels bound to this agent, grouped by platform. Admins also see inactive
 * bound channels and the edit/create affordances that manage credentials.
 *
 * Linking your own chat account is per platform, not per channel, so it lives
 * in each platform's group header ("my account: …") instead of a section
 * competing with the channel rows.
 */
export function AgentChannelsPanel({ agentId }: Props) {
  const { t } = useI18n();
  const { showToast } = useToast();
  const queryClient = useQueryClient();
  const { data: me } = useQuery(meQueryOptions);
  const isAdmin = me?.is_admin ?? false;

  // The two lists answer the same question for different viewers: admins get
  // every channel (including inactive ones), everyone else the active channels
  // whose binding they may see.
  const publicChannels = useQuery({ ...publicChannelsQueryOptions, enabled: !isAdmin });
  const adminChannels = useQuery({ ...channelsQueryOptions, enabled: isAdmin });
  const identities = useQuery(profileIdentitiesQueryOptions);

  const [pendingUnlink, setPendingUnlink] = useState<Identity | null>(null);
  const [pendingChannelUnbind, setPendingChannelUnbind] = useState<ChannelRow | null>(null);
  // Editing a channel happens here rather than on /settings/channels: leaving
  // the profile to rename a channel loses the agent you were configuring.
  const [editing, setEditing] = useState<NormalizedChannel | null>(null);
  const [editKey, setEditKey] = useState(0);
  // Same reasoning for creation: the new channel's agent is the one on screen.
  const [creating, setCreating] = useState(false);
  const [createKey, setCreateKey] = useState(0);

  const openEditor = (id: string) => {
    const channel = (adminChannels.data ?? []).find((ch) => ch.id === id);
    if (!channel) return;
    setEditing(normalizeChannel(channel));
    setEditKey((key) => key + 1);
  };

  const invalidateIdentities = () =>
    void queryClient.invalidateQueries({ queryKey: ["profile-identities"] });

  const link = useAccountLink({ notify: showToast, onLinked: invalidateIdentities });

  const unlink = useMutation({
    mutationFn: (identity: Identity) =>
      unlinkProfileIdentity({ path: { id: identity.id }, throwOnError: true }),
    onSuccess: async () => {
      showToast(t("agents.channels.unlinked"));
      link.reset();
      await queryClient.invalidateQueries({ queryKey: ["profile-identities"] });
    },
    onError: (error) =>
      showToast(apiErrorMessage(error, t("agents.channels.unlinkFailed")), "error"),
  });

  // The dedicated endpoint clears only agent_id, so this tab never becomes a
  // second writer of channel credentials.
  const unbindChannel = useMutation({
    mutationFn: (row: ChannelRow) =>
      bindChannelAgent({ path: { id: row.id }, body: { agent_id: "" }, throwOnError: true }),
    onSuccess: async () => {
      showToast(t("agents.channels.unbound"));
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["channels"] }),
        queryClient.invalidateQueries({ queryKey: ["public-channels"] }),
      ]);
    },
    onError: (error) => {
      const message = apiErrorMessage(error, t("agents.channels.bindFailed"));
      // The binding conflict explains itself and must reach the user verbatim;
      // the Agent PEP's bare "forbidden" explains nothing, so it gets words.
      showToast(message === "forbidden" ? t("agents.channels.bindForbidden") : message, "error");
    },
  });

  const rows = useMemo<ChannelRow[]>(() => {
    if (isAdmin) {
      return (adminChannels.data ?? [])
        .map((channel) => {
          // A row written before `type` existed carries its platform in the id,
          // the same fallback the backend applies.
          const type = channel.type || channel.id;
          return {
            id: channel.id,
            type,
            name: channel.name || platformLabel(type),
            agentId: channel.agent_id ?? "",
            is_active: channel.is_active ?? false,
          };
        })
        .filter((channel) => channel.agentId === agentId);
    }
    return (publicChannels.data ?? [])
      .map((channel) => ({
        id: channel.id,
        type: channel.type,
        name: platformLabel(channel.type, channel.label),
        agentId: channel.agent_id ?? "",
        is_active: channel.is_active,
      }))
      .filter((channel) => channel.agentId === agentId);
  }, [agentId, isAdmin, adminChannels.data, publicChannels.data]);

  // One group per platform: an account is linked per platform, so the link
  // affordance belongs to the group while the rows carry the routing.
  const groups = useMemo<PlatformGroup[]>(() => {
    const byType = new Map<string, PlatformGroup>();
    for (const row of rows) {
      const group = byType.get(row.type) ?? {
        type: row.type,
        label: platformLabel(row.type),
        rows: [],
      };
      group.rows.push(row);
      byType.set(row.type, group);
    }
    for (const group of byType.values()) {
      group.rows.sort((a, b) => {
        // The platform's default instance (id === type) leads its group.
        const aDefault = a.id === a.type;
        const bDefault = b.id === b.type;
        if (aDefault !== bDefault) return aDefault ? -1 : 1;
        return a.id.localeCompare(b.id);
      });
    }
    return [...byType.values()].sort((a, b) => a.label.localeCompare(b.label));
  }, [rows]);

  const identityFor = (platform: string) =>
    (identities.data ?? []).find((identity) => identity.platform === platform) ?? null;

  const isLoading = isAdmin ? adminChannels.isLoading : publicChannels.isLoading;

  return (
    <div className="flex flex-col gap-6">
      <ProfilePanelSection
        title={t("agents.channels.title")}
        description={t("agents.channels.desc")}
        count={rows.length}
        action={
          isAdmin ? (
            // Creation started here already knows which agent it serves, so it
            // stays on this page instead of routing to /settings/channels.
            <Button
              variant="outline"
              size="sm"
              onClick={() => {
                setCreateKey((key) => key + 1);
                setCreating(true);
              }}
            >
              <Plus size={16} />
              {t("channels.addChannel")}
            </Button>
          ) : undefined
        }
      >
        <div className="flex flex-col gap-4">
          {isLoading ? (
            <ProfileSectionMessage>{t("agents.channels.loading")}</ProfileSectionMessage>
          ) : groups.length === 0 ? (
            <ProfileSectionMessage>{t("agents.channels.empty")}</ProfileSectionMessage>
          ) : (
            groups.map((group) => {
              const identity = identityFor(group.type);
              const canLinkCode = LINK_CODE_PLATFORMS.has(group.type);
              const canScan = group.type === QR_PLATFORM;
              const pending = link.platform === group.type;
              return (
                <div key={group.type} className="flex flex-col gap-2">
                  <div className="flex min-w-0 items-center gap-2">
                    <span className="shrink-0 text-muted-foreground">
                      <PlatformIcon type={group.type} />
                    </span>
                    <span className="truncate text-sm font-semibold text-foreground">
                      {group.label}
                    </span>
                    <span className="shrink-0 text-xs text-muted-foreground">
                      {group.rows.length}
                    </span>
                    {/* Your account is a platform-level fact, so it lives in the
                        platform header with an explicit subject — dangling under
                        the rows it read as belonging to nothing. */}
                    {(canLinkCode || canScan) && (
                      <div className="ml-auto flex min-w-0 shrink-0 items-center gap-1">
                        {identity ? (
                          <>
                            {/* The raw platform id is machine noise; it stays
                                reachable on hover for support questions. */}
                            <span
                              className="max-w-48 truncate text-xs text-muted-foreground"
                              title={identity.external_id}
                            >
                              {t("agents.channels.linkedAs", {
                                name: identity.name || identity.external_id,
                              })}
                            </span>
                            <Button
                              variant="ghost"
                              size="xs"
                              disabled={unlink.isPending}
                              onClick={() => setPendingUnlink(identity)}
                            >
                              {t("agents.channels.unlink")}
                            </Button>
                          </>
                        ) : (
                          <Button
                            variant="ghost"
                            size="xs"
                            title={t("agents.channels.linkPrompt")}
                            loading={pending && (canScan ? link.qrPolling : link.generating)}
                            onClick={() =>
                              void (canScan ? link.startQr() : link.generateCode(group.type))
                            }
                          >
                            {t("agents.channels.linkAccount", { platform: group.label })}
                          </Button>
                        )}
                      </div>
                    )}
                  </div>

                  {group.rows.map((row) => {
                    const busy = unbindChannel.isPending && unbindChannel.variables?.id === row.id;
                    // The id only earns a line when it says something the name
                    // doesn't; the badge already carries the binding, so the
                    // subtitle never repeats it.
                    const subtitle = [
                      row.id !== row.name ? row.id : "",
                      isAdmin && !row.is_active ? t("channels.inactive") : "",
                    ]
                      .filter(Boolean)
                      .join(" · ");
                    return (
                      <div
                        key={row.id}
                        className="flex items-start justify-between gap-3 rounded-lg border border-border p-3"
                      >
                        <div className="flex min-w-0 flex-col gap-1">
                          <div className="flex min-w-0 items-center gap-2">
                            <span className="truncate text-sm font-medium text-foreground">
                              {row.name}
                            </span>
                            <Badge variant="success" size="sm">
                              {t("agents.channels.boundHere")}
                            </Badge>
                          </div>
                          {subtitle && (
                            <p className="truncate text-xs text-muted-foreground">{subtitle}</p>
                          )}
                        </div>
                        <div className="flex shrink-0 items-center gap-1">
                          {isAdmin && (
                            <Button
                              variant="ghost"
                              size="icon-sm"
                              aria-label={t("common.edit")}
                              onClick={() => openEditor(row.id)}
                            >
                              <Pencil size={16} />
                            </Button>
                          )}
                          <Button
                            variant="ghost"
                            size="sm"
                            loading={busy}
                            onClick={() => setPendingChannelUnbind(row)}
                          >
                            {t("agents.channels.unbind")}
                          </Button>
                        </div>
                      </div>
                    );
                  })}

                  {pending && link.code && (
                    <div className="flex flex-col gap-2 rounded-lg bg-muted p-3">
                      <p className="text-xs text-muted-foreground">
                        {t("agents.channels.linkHint", { platform: group.label })}
                      </p>
                      <div className="flex flex-wrap items-center gap-2">
                        <code className="select-all rounded bg-background px-2 py-1 font-mono text-sm text-foreground">
                          /link {link.code}
                        </code>
                        <Button variant="ghost" size="xs" onClick={link.copyCode}>
                          {t("common.copy")}
                        </Button>
                      </div>
                      <p className="text-xs text-muted-foreground">
                        {t("agents.channels.linkExpires")}
                      </p>
                    </div>
                  )}

                  {pending && link.qrUrl && (
                    <div className="flex flex-col items-center gap-2 rounded-lg bg-muted p-3">
                      <p className="text-xs text-muted-foreground">
                        {t("agents.channels.scanHint")}
                      </p>
                      <img
                        src={link.qrUrl}
                        alt={t("agents.channels.qrAlt")}
                        className="size-48 rounded-lg"
                      />
                      <Badge variant={weixinQrStatusVariant(link.qrStatus)}>
                        {t(qrStatusLabel(link.qrStatus))}
                      </Badge>
                      {link.qrStatus === "expired" && (
                        <Button variant="outline" size="xs" onClick={() => void link.startQr()}>
                          {t("common.refresh")}
                        </Button>
                      )}
                    </div>
                  )}
                </div>
              );
            })
          )}
        </div>
      </ProfilePanelSection>

      <ChannelCreateSheet
        open={creating}
        agentId={agentId}
        formKey={createKey}
        onOpenChange={setCreating}
        notify={showToast}
      />

      <ChannelEditSheet
        open={!!editing}
        channel={editing}
        formKey={editKey}
        onOpenChange={(open) => !open && setEditing(null)}
        notify={showToast}
      />

      {/* Both confirmations live at page level: an overlay nested inside another
          overlay is a bug (see web-ui.md). */}
      <AlertDialog open={!!pendingUnlink} onOpenChange={(open) => !open && setPendingUnlink(null)}>
        <AlertDialogPopup>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("agents.channels.unlinkTitle")}</AlertDialogTitle>
            <AlertDialogDescription>
              {t("agents.channels.unlinkConfirm", {
                platform: platformLabel(pendingUnlink?.platform ?? ""),
              })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogClose render={<Button variant="ghost" />}>
              {t("common.cancel")}
            </AlertDialogClose>
            <Button
              variant="destructive"
              onClick={() => {
                const target = pendingUnlink;
                setPendingUnlink(null);
                if (target) unlink.mutate(target);
              }}
            >
              {t("agents.channels.unlink")}
            </Button>
          </AlertDialogFooter>
        </AlertDialogPopup>
      </AlertDialog>

      <AlertDialog
        open={!!pendingChannelUnbind}
        onOpenChange={(open) => !open && setPendingChannelUnbind(null)}
      >
        <AlertDialogPopup>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("agents.channels.unbindTitle")}</AlertDialogTitle>
            <AlertDialogDescription>
              {t("agents.channels.unbindConfirm", { channel: pendingChannelUnbind?.name ?? "" })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogClose render={<Button variant="ghost" />}>
              {t("common.cancel")}
            </AlertDialogClose>
            <Button
              variant="destructive"
              onClick={() => {
                const target = pendingChannelUnbind;
                setPendingChannelUnbind(null);
                if (target) unbindChannel.mutate(target);
              }}
            >
              {t("agents.channels.unbind")}
            </Button>
          </AlertDialogFooter>
        </AlertDialogPopup>
      </AlertDialog>
    </div>
  );
}
