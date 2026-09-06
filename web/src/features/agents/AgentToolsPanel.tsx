import { useState, type ReactNode } from "react";
import { Link } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronRight, MoreHorizontal, Plus } from "lucide-react";
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
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Collapsible, CollapsiblePanel, CollapsibleTrigger } from "@/components/ui/collapsible";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuTrigger,
} from "@/components/ui/menu";
import { Switch } from "@/components/ui/switch";
import { Tooltip, TooltipPopup, TooltipTrigger } from "@/components/ui/tooltip";
import { updateAgentTool } from "@/lib/api-client";
import {
  deletePluginConfig,
  disconnectPluginConfigOAuth,
  getPluginConfig,
  startPluginConfigOAuth,
  updatePluginConfig,
} from "@/lib/api-client/sdk.gen";
import type { AgentMcpServer, PluginConfig } from "@/lib/api-client/types.gen";
import { apiErrorMessage } from "@/lib/api-error";
import { agentToolsOptions } from "@/lib/queries/agents";
import { agentMcpServersOptions } from "@/lib/queries/mcp";
import { meQueryOptions } from "@/lib/queries/me";
import { SCOPE_LABEL_KEY } from "@/lib/skill-scope";
import type { MessageKey } from "@/lib/i18n/messages";
import type { Tool } from "@/lib/types";
import { useToast } from "@/hooks/use-toast";
import { useI18n } from "@/lib/i18n";
import { AgentMcpServerSheet } from "./AgentMcpServerSheet";
import { ProfilePanelSection, ProfileSectionMessage } from "./ProfilePanelSection";

const SOURCE_LABEL_KEY = {
  core: "agents.tools.source.core",
  builtin: "agents.tools.source.builtin",
  plugin: "agents.tools.source.plugin",
  mcp: "agents.tools.source.mcp",
} as const;

const SYSTEM_FAMILY_ORDER = [
  "agent_management",
  "knowledge_and_skills",
  "models_and_deployment",
  "extensions_and_connections",
] as const;

const SYSTEM_FAMILY_LABEL_KEY = {
  agent_management: "agents.tools.system.family.agentManagement",
  knowledge_and_skills: "agents.tools.system.family.knowledgeAndSkills",
  models_and_deployment: "agents.tools.system.family.modelsAndDeployment",
  extensions_and_connections: "agents.tools.system.family.extensionsAndConnections",
} as const satisfies Record<(typeof SYSTEM_FAMILY_ORDER)[number], MessageKey>;

const REGULAR_FAMILY_ORDER = [
  "goal",
  "scheduler",
  "workflow",
  "oauth",
  "email",
  "share",
  "vault",
  "recally",
  "session",
  "skill",
  "library",
  "memory",
  "core_tools",
  "plugin_tools",
  "other_tools",
] as const;

const REGULAR_FAMILY_LABEL_KEY = {
  goal: "agents.tools.family.goal",
  scheduler: "agents.tools.family.scheduler",
  workflow: "agents.tools.family.workflow",
  oauth: "agents.tools.family.oauth",
  email: "agents.tools.family.email",
  share: "agents.tools.family.share",
  vault: "agents.tools.family.vault",
  recally: "agents.tools.family.recally",
  session: "agents.tools.family.session",
  skill: "agents.tools.family.skill",
  library: "agents.tools.family.library",
  memory: "agents.tools.family.memory",
  core_tools: "agents.tools.family.coreTools",
  plugin_tools: "agents.tools.family.pluginTools",
  other_tools: "agents.tools.family.otherTools",
} as const satisfies Record<(typeof REGULAR_FAMILY_ORDER)[number], MessageKey>;

type ToolOverrideScope = "user" | "user_agent" | "system" | "system_agent";
type SystemFamily = (typeof SYSTEM_FAMILY_ORDER)[number];
type RegularToolFamily = (typeof REGULAR_FAMILY_ORDER)[number];

const WIDER_SCOPES: ToolOverrideScope[] = ["user", "system_agent", "system"];
const ADMIN_SCOPES = new Set<string>(["system", "system_agent"]);
const EMAIL_CONFIG_REQUIRED = "email_config_required";
const MCP_SOURCE = "mcp";
const MCP_AVAILABILITY_REASONS = [
  "mcp_server_disabled",
  "mcp_server_error",
  "mcp_needs_auth",
] as const;
type McpAvailabilityReason = (typeof MCP_AVAILABILITY_REASONS)[number];

function mcpStatusKey(status: string): MessageKey {
  // SAFETY: status is untrusted API data; an unknown value renders as Unknown.
  const key = `mcp.status.${status}` as MessageKey;
  return key;
}

function mcpAvailabilityReason(reason: string): McpAvailabilityReason | null {
  // SAFETY: the reason list is a closed enum; anything else renders nothing
  // rather than inventing a backend identifier.
  return MCP_AVAILABILITY_REASONS.includes(reason as McpAvailabilityReason)
    ? (reason as McpAvailabilityReason)
    : null;
}
const FAMILY_UPDATE_CONCURRENCY = 4;

function pluginPath(pluginID: string) {
  const slash = pluginID.indexOf("/");
  if (slash <= 0 || slash === pluginID.length - 1) throw new Error("invalid plugin id");
  return { kind: pluginID.slice(0, slash), name: pluginID.slice(slash + 1) };
}

async function readOwnedConfig(server: AgentMcpServer): Promise<PluginConfig> {
  if (!server.readable) throw new Error("configuration is not readable");
  const { data } = await getPluginConfig({
    path: { ...pluginPath(server.plugin_id), config_id: server.config_id },
    throwOnError: true,
  });
  if (!data) throw new Error("configuration is unavailable");
  return data;
}

// A plugin can contribute an arbitrary number of tools. Keep the convenience
// fan-out bounded, and wait for every started write before the caller refetches.
export async function runBoundedFamilyUpdates<T>(
  items: T[],
  update: (item: T) => Promise<void>,
): Promise<void> {
  let next = 0;
  const errors: unknown[] = [];
  const worker = async () => {
    while (next < items.length) {
      const item = items[next++];
      try {
        await update(item);
      } catch (error) {
        errors.push(error);
      }
    }
  };
  await Promise.all(
    Array.from({ length: Math.min(FAMILY_UPDATE_CONCURRENCY, items.length) }, worker),
  );
  if (errors.length > 0) {
    throw errors[0];
  }
}

type FamilyState =
  | { kind: "email_config_required"; enabledCount: number; overrideCount: number }
  | { kind: "all_enabled"; enabledCount: number; overrideCount: number }
  | { kind: "partially_enabled"; enabledCount: number; overrideCount: number }
  | { kind: "all_disabled"; enabledCount: number; overrideCount: number }
  | { kind: "system_managed"; enabledCount: number; overrideCount: number };

interface Props {
  agentId: string;
  canEdit: boolean;
}

function isSystemSettingsTool(tool: Tool): tool is Tool & { family: SystemFamily } {
  return (
    tool.control === "system" &&
    tool.policy_reason === "settings_policy" &&
    tool.family != null &&
    // SAFETY: Settings policy remains a closed display-family list even though
    // AgentTool.family now also carries open-ended toolmeta families.
    SYSTEM_FAMILY_ORDER.includes(tool.family as SystemFamily)
  );
}

function sourceLabel(source: string): MessageKey {
  // SAFETY: source is untrusted API data, and a missing map entry deliberately
  // renders the generic Unknown label rather than claiming a different source.
  return SOURCE_LABEL_KEY[source as keyof typeof SOURCE_LABEL_KEY] ?? "agents.tools.source.unknown";
}

function regularFamily(tool: Tool): RegularToolFamily {
  // SAFETY: family is untrusted API data; membership in the label map narrows
  // it to the only display families this client translates explicitly.
  const family = tool.family as RegularToolFamily | undefined;
  if (family && family in REGULAR_FAMILY_LABEL_KEY) return family;
  // A new generated family or an untrusted plugin value must not turn into a
  // raw backend identifier in the UI. The backend groups known plugin tools
  // under plugin_tools; this catches future or malformed values safely.
  return "other_tools";
}

// groupedRegularTools is the Profile's single family-navigation boundary: source
// stays on each row as metadata and cannot create a second top-level section.
export function groupedRegularTools(
  tools: Tool[],
): Array<{ family: RegularToolFamily; tools: Tool[] }> {
  const members = new Map<RegularToolFamily, Tool[]>();
  for (const tool of tools) {
    const family = regularFamily(tool);
    const group = members.get(family) ?? [];
    group.push(tool);
    members.set(family, group);
  }
  return REGULAR_FAMILY_ORDER.filter((family) => members.has(family)).map((family) => ({
    family,
    tools: (members.get(family) ?? []).sort((a, b) => a.name.localeCompare(b.name)),
  }));
}

function familyState(tools: Tool[]): FamilyState {
  const overrides = tools.filter((tool) => tool.control === "override" && tool.enabled != null);
  const enabledCount = overrides.filter((tool) => tool.enabled).length;
  if (
    tools.length > 0 &&
    tools.every(
      (tool) =>
        tool.control === "system" &&
        tool.policy_reason === "runtime_unavailable" &&
        tool.availability_reason === EMAIL_CONFIG_REQUIRED,
    )
  ) {
    return { kind: "email_config_required", enabledCount, overrideCount: overrides.length };
  }
  if (overrides.length === 0) {
    return { kind: "system_managed", enabledCount, overrideCount: 0 };
  }
  if (enabledCount === overrides.length) {
    return { kind: "all_enabled", enabledCount, overrideCount: overrides.length };
  }
  if (enabledCount === 0) {
    return { kind: "all_disabled", enabledCount, overrideCount: overrides.length };
  }
  return { kind: "partially_enabled", enabledCount, overrideCount: overrides.length };
}

function originLabel(origin: string): MessageKey {
  if (origin === "default") return "agents.tools.origin.default";
  // SAFETY: origin is untrusted API data, and an unknown scope must render as
  // Unknown rather than the default origin.
  return SCOPE_LABEL_KEY[origin as keyof typeof SCOPE_LABEL_KEY] ?? "agents.tools.origin.unknown";
}

/**
 * The profile is a control surface, not a runtime preview. System Settings are
 * described from server policy, while only rows marked `override` can write a
 * tool override that the runner will honor.
 */
export function AgentToolsPanel({ agentId, canEdit }: Props) {
  const { t } = useI18n();
  const { showToast } = useToast();
  const queryClient = useQueryClient();
  const { data: me } = useQuery(meQueryOptions);
  const isAdmin = me?.is_admin ?? false;
  const query = useQuery(agentToolsOptions(agentId));
  const [sheetOpen, setSheetOpen] = useState(false);
  const [editingServer, setEditingServer] = useState<AgentMcpServer | null>(null);
  const [formSeq, setFormSeq] = useState(0);
  const [pendingDelete, setPendingDelete] = useState<AgentMcpServer | null>(null);
  const mcpQuery = useQuery(agentMcpServersOptions(agentId));

  const invalidateMcp = async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ["agent-tools", agentId] }),
      queryClient.invalidateQueries({ queryKey: ["agent-mcp-servers", agentId] }),
    ]);
  };

  const removeServer = useMutation({
    mutationFn: async (server: AgentMcpServer) => {
      const config = await readOwnedConfig(server);
      return deletePluginConfig({
        path: { ...pluginPath(server.plugin_id), config_id: server.config_id },
        query: { expected_revision: config.revision },
        throwOnError: true,
      });
    },
    onSuccess: async () => {
      showToast(t("mcp.deleted"), "success");
      await invalidateMcp();
    },
    onError: (error) => showToast(apiErrorMessage(error, t("mcp.deleteFailed")), "error"),
  });

  const connectServer = useMutation({
    mutationFn: (server: AgentMcpServer) =>
      startPluginConfigOAuth({
        path: { ...pluginPath(server.plugin_id), config_id: server.config_id },
        throwOnError: true,
      }),
    onSuccess: async ({ data }) => {
      if (data?.authorization_url) {
        // Navigate the whole tab: the external authorization server redirects
        // back to /settings/mcp, which toasts the outcome.
        window.location.href = data.authorization_url;
      }
    },
    onError: (error) => showToast(apiErrorMessage(error, t("mcp.connectFailed")), "error"),
  });

  const disconnectServer = useMutation({
    mutationFn: (server: AgentMcpServer) =>
      disconnectPluginConfigOAuth({
        path: { ...pluginPath(server.plugin_id), config_id: server.config_id },
        throwOnError: true,
      }),
    onSuccess: invalidateMcp,
    onError: (error) => showToast(apiErrorMessage(error, t("mcp.disconnectFailed")), "error"),
  });

  const toggleServer = useMutation({
    mutationFn: async ({ server, enabled }: { server: AgentMcpServer; enabled: boolean }) => {
      const config = await readOwnedConfig(server);
      return updatePluginConfig({
        path: { ...pluginPath(server.plugin_id), config_id: server.config_id },
        body: { expected_revision: config.revision, is_enabled: enabled },
        throwOnError: true,
      });
    },
    onSuccess: invalidateMcp,
    onError: () => showToast(t("agents.tools.updateFailed"), "error"),
  });

  const openServerSheet = (server: AgentMcpServer | null) => {
    setEditingServer(server);
    setFormSeq((n) => n + 1);
    setSheetOpen(true);
  };

  const invalidateTools = async () => {
    await queryClient.invalidateQueries({ queryKey: ["agent-tools", agentId] });
  };

  const mutation = useMutation({
    mutationFn: ({
      tool,
      enabled,
      scope,
    }: {
      tool: Tool;
      enabled: boolean;
      scope: ToolOverrideScope;
    }) =>
      updateAgentTool({
        path: { id: agentId, toolName: tool.name },
        body: { enabled, scope },
        throwOnError: true,
      }),
    onSuccess: invalidateTools,
    onError: () => showToast(t("agents.tools.updateFailed"), "error"),
  });

  const familyMutation = useMutation({
    mutationFn: async ({
      family,
      tools,
      enabled,
    }: {
      family: RegularToolFamily;
      tools: Tool[];
      enabled: boolean;
    }) => {
      // Each row retains the existing, scoped override endpoint. The family
      // action is only a bounded convenience fan-out, never a second policy path.
      await runBoundedFamilyUpdates(tools, async (tool) => {
        await updateAgentTool({
          path: { id: agentId, toolName: tool.name },
          body: { enabled, scope: "user_agent" },
          throwOnError: true,
        });
      });
      return { family, enabled };
    },
    onSuccess: ({ enabled }) => {
      showToast(
        t(enabled ? "agents.tools.family.enabledAll" : "agents.tools.family.disabledAll"),
        "success",
      );
    },
    onError: () => showToast(t("agents.tools.family.updateFailed"), "error"),
    // A family can contain a tool whose runtime dependency changed while these
    // writes were in flight. Always refetch the server's effective state.
    onSettled: invalidateTools,
  });

  const mcpFamilyMutation = useMutation({
    mutationFn: async ({
      family,
      tools,
      enabled,
    }: {
      family: string;
      tools: Tool[];
      enabled: boolean;
    }) => {
      // Same bounded fan-out as regular families; the MCP rows go through the
      // identical override endpoint, never a second policy path.
      await runBoundedFamilyUpdates(tools, async (tool) => {
        await updateAgentTool({
          path: { id: agentId, toolName: tool.name },
          body: { enabled, scope: "user_agent" },
          throwOnError: true,
        });
      });
      return { family, enabled };
    },
    onSuccess: ({ enabled }) => {
      showToast(
        t(enabled ? "agents.tools.family.enabledAll" : "agents.tools.family.disabledAll"),
        "success",
      );
    },
    onError: () => showToast(t("agents.tools.family.updateFailed"), "error"),
    onSettled: invalidateTools,
  });

  if (!agentId) {
    return <ProfileSectionMessage>{t("agents.tools.createFirst")}</ProfileSectionMessage>;
  }
  if (query.isLoading) {
    return <ProfileSectionMessage>{t("agents.tools.loading")}</ProfileSectionMessage>;
  }
  if (query.isError) {
    return <ProfileSectionMessage>{t("agents.tools.loadFailed")}</ProfileSectionMessage>;
  }

  const catalog = query.data ?? [];
  const systemSettings = catalog.filter(isSystemSettingsTool);
  const tools = catalog.filter((tool) => tool.source !== "mcp" && !isSystemSettingsTool(tool));
  const toolFamilies = groupedRegularTools(tools);

  return (
    <div className="flex flex-col gap-6">
      {canEdit && (
        <AgentMcpServerSheet
          agentId={agentId}
          isAdmin={isAdmin}
          open={sheetOpen}
          server={editingServer}
          formKey={formSeq}
          onOpenChange={setSheetOpen}
          notify={showToast}
        />
      )}
      <AlertDialog open={!!pendingDelete} onOpenChange={(open) => !open && setPendingDelete(null)}>
        <AlertDialogPopup>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("mcp.deleteTitle")}</AlertDialogTitle>
            <AlertDialogDescription>
              {t("mcp.deleteConfirm", { name: pendingDelete?.namespace ?? "" })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogClose render={<Button variant="ghost" />}>
              {t("common.cancel")}
            </AlertDialogClose>
            <Button
              variant="destructive"
              onClick={() => {
                const target = pendingDelete;
                setPendingDelete(null);
                if (target) removeServer.mutate(target);
              }}
            >
              {t("common.delete")}
            </Button>
          </AlertDialogFooter>
        </AlertDialogPopup>
      </AlertDialog>

      {canEdit && (
        <SystemSettingsSection
          agentId={agentId}
          enabled={systemSettings.length > 0}
          tools={systemSettings}
        />
      )}

      <ProfilePanelSection
        title={t("agents.tools.title")}
        count={tools.length}
        description={t("agents.tools.description")}
      >
        {toolFamilies.length === 0 ? (
          <ProfileSectionMessage>{t("agents.tools.empty")}</ProfileSectionMessage>
        ) : (
          <div className="flex flex-col gap-3">
            {toolFamilies.map(({ family, tools: members }) => (
              <RegularToolFamilyCard
                key={family}
                family={family}
                tools={members}
                defaultOpen={false}
                canEdit={canEdit}
                isAdmin={isAdmin}
                busyToolName={mutation.isPending ? (mutation.variables?.tool.name ?? null) : null}
                familyBusy={familyMutation.isPending && familyMutation.variables?.family === family}
                onToggle={(tool, enabled, scope) => mutation.mutate({ tool, enabled, scope })}
                onSetFamilyEnabled={(members, enabled) =>
                  familyMutation.mutate({ family, tools: members, enabled })
                }
              />
            ))}
          </div>
        )}
      </ProfilePanelSection>

      <ProfilePanelSection
        title={t("agents.tools.mcpServers")}
        count={(mcpQuery.data ?? []).length}
        description={t("agents.tools.mcpDescription")}
        action={
          canEdit && (
            <Button
              variant="ghost"
              size="icon-sm"
              aria-label={t("mcp.addTitle")}
              title={t("mcp.addTitle")}
              onClick={() => openServerSheet(null)}
            >
              <Plus />
            </Button>
          )
        }
      >
        <div className="flex flex-col gap-2">
          {(mcpQuery.data ?? []).length === 0 ? (
            <ProfileSectionMessage>{t("mcp.empty")}</ProfileSectionMessage>
          ) : (
            (mcpQuery.data ?? []).map((server) => {
              const members = (query.data ?? []).filter(
                (tool) => tool.source === MCP_SOURCE && tool.family === `mcp:${server.namespace}`,
              );
              return (
                <McpServerGroup
                  key={`mcp:${server.config_id}`}
                  server={server}
                  tools={members}
                  canEdit={canEdit}
                  isAdmin={isAdmin}
                  busyToolName={mutation.isPending ? (mutation.variables?.tool.name ?? null) : null}
                  familyBusy={
                    mcpFamilyMutation.isPending &&
                    mcpFamilyMutation.variables?.family === `mcp:${server.namespace}`
                  }
                  toggleBusy={
                    toggleServer.isPending &&
                    toggleServer.variables?.server.config_id === server.config_id
                  }
                  onToggle={(tool, enabled, scope) => mutation.mutate({ tool, enabled, scope })}
                  onSetFamilyEnabled={(members_, enabled) =>
                    mcpFamilyMutation.mutate({
                      family: `mcp:${server.namespace}`,
                      tools: members_,
                      enabled,
                    })
                  }
                  onToggleServer={(enabled) => toggleServer.mutate({ server, enabled })}
                  onEdit={openServerSheet}
                  onDelete={setPendingDelete}
                  onConnect={(srv) => connectServer.mutate(srv)}
                  onDisconnect={(srv) => disconnectServer.mutate(srv)}
                />
              );
            })
          )}
        </div>
      </ProfilePanelSection>
    </div>
  );
}

export function SystemSettingsSection({
  agentId,
  enabled,
  tools,
}: {
  agentId: string;
  enabled: boolean;
  tools: Array<Tool & { family: SystemFamily }>;
}) {
  const { t } = useI18n();
  return (
    <ProfilePanelSection
      title={t("agents.tools.system.title")}
      count={enabled ? tools.length : undefined}
      description={t("agents.tools.system.description")}
    >
      {!enabled ? (
        <div className="flex flex-wrap items-center justify-between gap-2">
          <p className="text-xs text-muted-foreground">{t("agents.tools.system.disabled")}</p>
          <Button
            variant="link"
            size="xs"
            render={
              <Link to="/agents/$agentId/profile" params={{ agentId }} search={{ tab: "config" }} />
            }
          >
            {t("agents.tools.system.configure")}
          </Button>
        </div>
      ) : (
        <>
          <div className="flex flex-wrap gap-2">
            <Badge variant="outline">{t("agents.tools.system.badge.foregroundOnly")}</Badge>
            <Badge variant="outline">{t("agents.tools.system.badge.writeChecks")}</Badge>
            <Badge variant="outline">{t("agents.tools.system.badge.credentials")}</Badge>
          </div>
          <p className="text-xs text-muted-foreground">{t("agents.tools.system.policy")}</p>
          {tools.length === 0 ? (
            <ProfileSectionMessage>{t("agents.tools.system.empty")}</ProfileSectionMessage>
          ) : (
            <div className="flex flex-col gap-3">
              {SYSTEM_FAMILY_ORDER.map((family) => {
                const members = tools.filter((tool) => tool.family === family);
                if (members.length === 0) return null;
                return (
                  <ToolFamilyCard
                    key={family}
                    title={t(SYSTEM_FAMILY_LABEL_KEY[family])}
                    defaultOpen={false}
                    badges={
                      <>
                        <Badge variant="outline">
                          {t("agents.tools.family.actionCount", { count: members.length })}
                        </Badge>
                        <Badge variant="outline">{t("agents.tools.system.readOnly")}</Badge>
                      </>
                    }
                  >
                    {members.map((tool) => (
                      <SettingsActionRow key={tool.name} tool={tool} />
                    ))}
                  </ToolFamilyCard>
                );
              })}
            </div>
          )}
        </>
      )}
    </ProfilePanelSection>
  );
}

export function RegularToolFamilyCard({
  family,
  tools,
  defaultOpen,
  canEdit,
  isAdmin,
  busyToolName,
  familyBusy,
  onToggle,
  onSetFamilyEnabled,
}: {
  family: RegularToolFamily;
  tools: Tool[];
  defaultOpen: boolean;
  canEdit: boolean;
  isAdmin: boolean;
  busyToolName: string | null;
  familyBusy: boolean;
  onToggle: (tool: Tool, enabled: boolean, scope: ToolOverrideScope) => void;
  onSetFamilyEnabled: (tools: Tool[], enabled: boolean) => void;
}) {
  const { t } = useI18n();
  const state = familyState(tools);
  const emailConfigRequired = state.kind === "email_config_required";
  const stateLabel =
    state.kind === "email_config_required"
      ? t("agents.tools.family.emailSetupRequired")
      : state.kind === "all_enabled"
        ? t("agents.tools.family.allEnabled")
        : state.kind === "partially_enabled"
          ? t("agents.tools.family.enabledCount", { count: state.enabledCount })
          : state.kind === "all_disabled"
            ? t("agents.tools.family.allDisabled")
            : t("agents.tools.systemManaged");
  const stateVariant =
    state.kind === "email_config_required"
      ? "warning"
      : state.kind === "all_enabled"
        ? "success"
        : "outline";
  const overrideTools = tools.filter(
    (tool) => tool.control === "override" && tool.enabled != null && tool.origin != null,
  );
  // A family action must never partially appear to defeat an admin-level off.
  // The affected row remains individually explained and locked instead.
  const hasAdminLock = overrideTools.some(
    (tool) => !tool.enabled && ADMIN_SCOPES.has(tool.origin ?? "default"),
  );
  // Plugin families have no cardinality ceiling. Keep them per-tool rather than
  // multiplying catalog scans through a deployment-sized fan-out.
  const canSetFamily =
    canEdit && family !== "plugin_tools" && overrideTools.length > 0 && !hasAdminLock;
  const nextFamilyEnabled = state.kind !== "all_enabled";

  return (
    <ToolFamilyCard
      title={t(REGULAR_FAMILY_LABEL_KEY[family])}
      defaultOpen={defaultOpen}
      badges={
        <>
          <Badge variant="outline">
            {t("agents.tools.family.actionCount", { count: tools.length })}
          </Badge>
          <Badge variant={stateVariant}>{stateLabel}</Badge>
          {canSetFamily && (
            <Button
              variant="secondary"
              size="xs"
              disabled={familyBusy}
              onClick={() => onSetFamilyEnabled(overrideTools, nextFamilyEnabled)}
            >
              {t(
                nextFamilyEnabled
                  ? "agents.tools.family.enableAll"
                  : "agents.tools.family.disableAll",
              )}
            </Button>
          )}
        </>
      }
      description={
        emailConfigRequired && (
          <>
            <span>{t("agents.tools.family.emailConfigRequired")}</span>
            <Button variant="link" size="xs" render={<Link to="/settings/credentials" />}>
              {t("agents.tools.family.configureEmail")}
            </Button>
          </>
        )
      }
    >
      {tools.map((tool) => (
        <ToolRow
          key={`${tool.source}:${tool.name}`}
          tool={tool}
          canEdit={canEdit}
          isAdmin={isAdmin}
          busy={busyToolName === tool.name}
          compactRuntimeStatus={emailConfigRequired}
          onToggle={(enabled, scope) => onToggle(tool, enabled, scope)}
        />
      ))}
    </ToolFamilyCard>
  );
}

/**
 * The one collapsible card every tool family on this tab folds into, whether
 * the rows inside are toggleable tools or the read-only settings-action
 * catalog. Title left, badges and any family action right, an optional
 * description line under the heading, member rows in the panel.
 */
function ToolFamilyCard({
  title,
  defaultOpen,
  badges,
  description,
  children,
}: {
  title: string;
  defaultOpen: boolean;
  badges: ReactNode;
  description?: ReactNode;
  children: ReactNode;
}) {
  return (
    <Collapsible defaultOpen={defaultOpen} render={<Card />}>
      <CardHeader>
        <h3 className="flex min-w-0">
          <CollapsibleTrigger className="group flex min-w-0 flex-1 items-center gap-2 text-left">
            <ChevronRight className="size-4 shrink-0 text-muted-foreground transition-transform duration-150 ease-out group-data-[panel-open]:rotate-90" />
            <CardTitle render={<span className="truncate" />}>{title}</CardTitle>
          </CollapsibleTrigger>
        </h3>
        <CardAction>
          <div className="flex items-center gap-1">{badges}</div>
        </CardAction>
        {description && (
          <CardDescription render={<div className="flex flex-wrap items-center gap-2" />}>
            {description}
          </CardDescription>
        )}
      </CardHeader>
      <CollapsiblePanel render={<CardContent />}>
        <div className="flex flex-col gap-2">{children}</div>
      </CollapsiblePanel>
    </Collapsible>
  );
}

function SettingsActionRow({ tool }: { tool: Tool }) {
  const { t } = useI18n();
  return (
    <Card>
      <CardContent className="flex min-w-0 flex-col gap-1">
        <div className="flex min-w-0 items-center gap-2">
          <span className="truncate font-mono text-sm font-semibold text-foreground">
            {tool.name}
          </span>
          {tool.admin_required && (
            <Badge variant="outline">{t("agents.tools.system.adminRequired")}</Badge>
          )}
          <Badge variant="outline">{t(sourceLabel(tool.source))}</Badge>
        </div>
        <p className="text-xs text-muted-foreground">{tool.description}</p>
      </CardContent>
    </Card>
  );
}

/**
 * One MCP server group: header carries the registration's lifecycle (status,
 * enable switch, edit/delete), rows are the server's tools with the same
 * per-tool override control as builtin rows. Unreadable servers (another
 * user's row visible only through the effective-context lens) stay read-only.
 */
export function McpServerGroup({
  server,
  tools,
  defaultOpen = false,
  canEdit,
  isAdmin,
  busyToolName,
  familyBusy,
  toggleBusy,
  onToggle,
  onSetFamilyEnabled,
  onToggleServer,
  onEdit,
  onDelete,
  onConnect,
  onDisconnect,
}: {
  server: AgentMcpServer;
  tools: Tool[];
  defaultOpen?: boolean;
  canEdit: boolean;
  isAdmin: boolean;
  busyToolName: string | null;
  familyBusy: boolean;
  toggleBusy: boolean;
  onToggle: (tool: Tool, enabled: boolean, scope: ToolOverrideScope) => void;
  onSetFamilyEnabled: (tools: Tool[], enabled: boolean) => void;
  onToggleServer: (enabled: boolean) => void;
  onEdit: (server: AgentMcpServer) => void;
  onDelete: (server: AgentMcpServer) => void;
  onConnect: (server: AgentMcpServer) => void;
  onDisconnect: (server: AgentMcpServer) => void;
}) {
  const { t } = useI18n();
  const overrideTools = tools.filter(
    (tool) => tool.control === "override" && tool.enabled != null && tool.origin != null,
  );
  const hasAdminLock = overrideTools.some(
    (tool) => !tool.enabled && ADMIN_SCOPES.has(tool.origin ?? "default"),
  );
  const canSetFamily = canEdit && overrideTools.length > 0 && !hasAdminLock;
  const nextFamilyEnabled =
    overrideTools.length === 0 || overrideTools.some((tool) => !tool.enabled);
  // The registration row, not the tool rows, owns the server-level health
  // signal: map it onto the same reason labels the tool rows carry.
  const reason = !server.enabled
    ? "mcp_server_disabled"
    : server.status !== "ok"
      ? mcpAvailabilityReason(
          server.status === "needs_auth" ? "mcp_needs_auth" : "mcp_server_error",
        )
      : null;

  return (
    <ToolFamilyCard
      title={server.namespace}
      defaultOpen={defaultOpen}
      badges={
        <>
          <Badge variant="outline">{t(SCOPE_LABEL_KEY[server.scope])}</Badge>
          <Badge
            variant={
              server.status === "ok" ? "success" : server.status === "error" ? "warning" : "outline"
            }
          >
            {t(mcpStatusKey(server.status))}
          </Badge>
          {canSetFamily && (
            <Button
              variant="secondary"
              size="xs"
              disabled={familyBusy}
              onClick={() => onSetFamilyEnabled(overrideTools, nextFamilyEnabled)}
            >
              {t(
                nextFamilyEnabled
                  ? "agents.tools.family.enableAll"
                  : "agents.tools.family.disableAll",
              )}
            </Button>
          )}
        </>
      }
      description={
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          <span className="truncate text-xs text-muted-foreground">{server.plugin_id}</span>
          {reason && <Badge variant="warning">{t(`agents.tools.mcp.reason.${reason}`)}</Badge>}
        </div>
      }
    >
      {canEdit && server.readable && (
        <div className="flex items-center justify-end gap-2 pr-1">
          <span className="text-xs text-muted-foreground">{t("agents.tools.mcp.server")}</span>
          <Switch
            checked={server.enabled}
            disabled={toggleBusy}
            onCheckedChange={(checked) => onToggleServer(!!checked)}
            aria-label={t("agents.tools.mcp.server")}
          />
          {(server.needs_auth || server.credential_mode === "per_user") && (
            <Button
              variant="outline"
              size="xs"
              disabled={toggleBusy}
              onClick={() => (server.needs_auth ? onConnect(server) : onDisconnect(server))}
            >
              {server.needs_auth ? t("mcp.connect") : t("mcp.disconnect")}
            </Button>
          )}
          <DropdownMenu>
            <DropdownMenuTrigger
              render={
                <Button
                  variant="ghost"
                  size="icon-xs"
                  disabled={toggleBusy}
                  aria-label={t("common.actions")}
                />
              }
            >
              <MoreHorizontal />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" sideOffset={6}>
              <DropdownMenuLabel>{server.namespace}</DropdownMenuLabel>
              <DropdownMenuItem onClick={() => onEdit(server)}>{t("common.edit")}</DropdownMenuItem>
              <DropdownMenuItem onClick={() => onDelete(server)}>
                {t("common.delete")}
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      )}
      {!server.readable && (server.needs_auth || server.credential_mode === "per_user") && (
        <div className="flex items-center justify-end gap-2 pr-1">
          <Button
            variant="outline"
            size="xs"
            onClick={() => (server.needs_auth ? onConnect(server) : onDisconnect(server))}
          >
            {server.needs_auth ? t("mcp.connect") : t("mcp.disconnect")}
          </Button>
        </div>
      )}
      {tools.length === 0 ? (
        <ProfileSectionMessage>{t("agents.tools.mcp.noTools")}</ProfileSectionMessage>
      ) : (
        tools.map((tool) => (
          <ToolRow
            key={`${tool.source}:${tool.name}`}
            tool={tool}
            canEdit={canEdit}
            isAdmin={isAdmin}
            busy={busyToolName === tool.name}
            onToggle={(enabled, scope) => onToggle(tool, enabled, scope)}
          />
        ))
      )}
    </ToolFamilyCard>
  );
}

export function ToolRow({
  tool,
  canEdit,
  isAdmin,
  busy,
  compactRuntimeStatus = false,
  onToggle,
}: {
  tool: Tool;
  canEdit: boolean;
  isAdmin: boolean;
  busy: boolean;
  compactRuntimeStatus?: boolean;
  onToggle: (enabled: boolean, scope: ToolOverrideScope) => void;
}) {
  const { t } = useI18n();
  const overridable = tool.control === "override" && tool.enabled != null && tool.origin != null;
  const enabled = tool.enabled ?? false;
  const origin = tool.origin ?? "default";
  const adminLocked = overridable && !enabled && ADMIN_SCOPES.has(origin) && !isAdmin;
  const scopes = WIDER_SCOPES.filter((scope) => isAdmin || !ADMIN_SCOPES.has(scope));

  return (
    <Card>
      <CardContent className="flex items-start justify-between gap-3">
        <div className="flex min-w-0 flex-col gap-1">
          <div className="flex min-w-0 items-center gap-2">
            <span className="truncate font-mono text-sm font-semibold text-foreground">
              {tool.name}
            </span>
            {overridable ? (
              <>
                <Badge variant={enabled ? "success" : "outline"}>
                  {enabled ? t("agents.tools.enabled") : t("agents.tools.disabled")}
                </Badge>
                <Badge variant="outline">{t(originLabel(origin))}</Badge>
              </>
            ) : (
              !compactRuntimeStatus && (
                <Badge variant="outline">{t("agents.tools.systemManaged")}</Badge>
              )
            )}
            <Badge variant="outline">{t(sourceLabel(tool.source))}</Badge>
          </div>
          <p className="text-xs text-muted-foreground">{tool.description}</p>
          {!overridable && tool.policy_reason === "core_sandbox" && (
            <p className="text-xs text-muted-foreground">{t("agents.tools.locked.core")}</p>
          )}
          {!overridable &&
            !compactRuntimeStatus &&
            tool.policy_reason === "runtime_unavailable" && (
              <p className="text-xs text-muted-foreground">{t("agents.tools.runtimeManaged")}</p>
            )}
        </div>
        {canEdit && overridable && (
          <div className="flex shrink-0 items-center gap-1">
            {adminLocked ? (
              <Tooltip>
                <TooltipTrigger render={<span className="inline-flex" />}>
                  <Switch checked={false} disabled />
                </TooltipTrigger>
                <TooltipPopup>{t("agents.tools.adminDisabled")}</TooltipPopup>
              </Tooltip>
            ) : (
              <>
                <Switch
                  checked={enabled}
                  disabled={busy}
                  onCheckedChange={(checked) => onToggle(!!checked, "user_agent")}
                />
                <DropdownMenu>
                  <DropdownMenuTrigger
                    render={
                      <Button
                        variant="ghost"
                        size="icon-xs"
                        disabled={busy}
                        aria-label={t("agents.tools.moreScopes")}
                      />
                    }
                  >
                    <MoreHorizontal />
                  </DropdownMenuTrigger>
                  <DropdownMenuContent align="end" sideOffset={6}>
                    <DropdownMenuLabel>
                      {enabled ? t("agents.tools.applyDisable") : t("agents.tools.applyEnable")}
                    </DropdownMenuLabel>
                    {scopes.map((scope) => (
                      <DropdownMenuItem key={scope} onClick={() => onToggle(!enabled, scope)}>
                        {t(SCOPE_LABEL_KEY[scope])}
                      </DropdownMenuItem>
                    ))}
                  </DropdownMenuContent>
                </DropdownMenu>
              </>
            )}
          </div>
        )}
      </CardContent>
    </Card>
  );
}
