import { useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useMutation, useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  createPlugin,
  createPluginConfig,
  disconnectPluginConfigOAuth,
  deletePluginConfig,
  deletePlugin,
  resetPluginConfig,
  startPluginConfigOAuth,
  updatePluginConfig,
} from "@/lib/api-client/sdk.gen";
import type {
  ComponentsCreatePluginRequestWritable,
  ComponentsPluginConfigInputWritable,
  ComponentsUpdatePluginConfigRequestWritable,
  PluginConfig,
  PluginDefinition,
} from "@/lib/api-client";
import {
  pluginConfigsQueryOptions,
  pluginsQueryOptions,
  type PluginScope,
} from "@/lib/queries/plugins";
import { agentsQueryOptions, allAgentsAdminQueryOptions } from "@/lib/queries/agents";
import { isAgentManagedScope, scopesForBand, type ScopeBand } from "@/lib/scope-band";
import { ErrorState } from "@/components/RouteFallback";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Field, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectItem,
  SelectPopup,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import {
  SettingsCardSection,
  SettingsDetailSheet,
  SettingsGridPage,
} from "@/features/settings/SettingsCardGrid";
import { ConfirmDialog } from "@/features/settings/ConfirmDialog";
import { DetailPanel, DetailPanelHeader } from "@/features/settings/SettingsDetailPanel";
import { useToast } from "@/hooks/use-toast";
import { useI18n } from "@/lib/i18n";
import { apiErrorCode, apiErrorMessage } from "@/lib/api-error";
import {
  PluginConfigEditor,
  type PluginConfigCredentials,
  type PluginConfigPayload,
} from "@/features/plugins/PluginConfigEditor";
import { McpInstallSheet } from "@/features/mcp/McpInstallSheet";
import { Cable, Code2, MessageSquare, Package, Plus, RotateCcw, Trash2 } from "lucide-react";

export type Translate = ReturnType<typeof useI18n>["t"];

const oauthClientInitializationMessage =
  "administrator must initialize this connection before users can authorize their own accounts";

export function pluginErrorMessage(error: unknown, t: Translate): string {
  const message = apiErrorMessage(error, t("common.error"));
  if (apiErrorCode(error) === 409 && message === oauthClientInitializationMessage) {
    return t("plugins.oauthAdminInitializationRequired");
  }
  return message;
}

function routeName(plugin: PluginDefinition): string {
  const slash = plugin.id.indexOf("/");
  return slash === -1 ? plugin.id : plugin.id.slice(slash + 1);
}

function routePath(plugin: PluginDefinition): { kind: string; name: string } {
  const slash = plugin.id.indexOf("/");
  return slash === -1
    ? { kind: plugin.backend, name: plugin.id }
    : { kind: plugin.id.slice(0, slash), name: plugin.id.slice(slash + 1) };
}

function backendIcon(plugin: PluginDefinition) {
  if (plugin.backend === "mcp") return <Cable className="size-4" />;
  if (plugin.backend === "cli") return <Package className="size-4" />;
  if (plugin.id.startsWith("channel/")) return <MessageSquare className="size-4" />;
  return <Code2 className="size-4" />;
}

function backendTitle(plugin: PluginDefinition, t: Translate): string {
  if (plugin.backend === "mcp") return t("plugins.backend.mcp");
  if (plugin.backend === "cli") return t("plugins.backend.cli");
  if (plugin.id.startsWith("channel/")) return t("plugins.backend.channel");
  return t("plugins.backend.go");
}

function scopeLabel(scope: PluginScope, t: Translate): string {
  return t(`plugins.scope.${scope}`);
}

function BackendSummary({ config, t }: { config: PluginConfig; t: Translate }) {
  const summary = config.backend_summary;
  if (summary.backend === "mcp") {
    return (
      <div className="flex flex-wrap gap-1.5">
        <Badge variant="outline" size="sm">
          {summary.transport}
        </Badge>
        <Badge variant="outline" size="sm">
          {summary.auth_type}
        </Badge>
        <Badge variant="outline" size="sm">
          {summary.credential_mode}
        </Badge>
        <Badge variant={summary.endpoint_configured ? "success" : "warning"} size="sm">
          {t(
            summary.endpoint_configured
              ? "plugins.summary.endpointReady"
              : "plugins.summary.endpointMissing",
          )}
        </Badge>
        {summary.auth_type === "bearer" && (
          <Badge variant={summary.bearer_configured ? "success" : "warning"} size="sm">
            {t(
              summary.bearer_configured
                ? "plugins.summary.bearerReady"
                : "plugins.summary.bearerMissing",
            )}
          </Badge>
        )}
        {summary.auth_type === "oauth" && (
          <>
            <Badge variant={summary.oauth_client_id_configured ? "success" : "warning"} size="sm">
              {t(
                summary.oauth_client_id_configured
                  ? "plugins.summary.oauthClientReady"
                  : "plugins.summary.oauthClientMissing",
              )}
            </Badge>
            <Badge
              variant={summary.oauth_client_secret_configured ? "success" : "warning"}
              size="sm"
            >
              {t(
                summary.oauth_client_secret_configured
                  ? "plugins.summary.oauthClientSecretReady"
                  : "plugins.summary.oauthClientSecretMissing",
              )}
            </Badge>
          </>
        )}
      </div>
    );
  }
  if (summary.backend === "cli") {
    const binaries = summary.binaries.map((binary) => `${binary.name} ${binary.version}`.trim());
    const skills = summary.skills.map((skill) => (
      <Badge key={skill.name} variant="outline" size="sm">
        {skill.name}
      </Badge>
    ));
    return (
      <div className="space-y-1">
        {binaries.length > 0 && (
          <p className="text-xs text-muted-foreground">{binaries.join(", ")}</p>
        )}
        {skills.length > 0 && <div className="flex flex-wrap gap-1.5">{skills}</div>}
        {summary.session_env.length > 0 && (
          <p className="text-xs text-muted-foreground">
            {t("plugins.summary.env", { count: summary.session_env.length })}
          </p>
        )}
        {summary.oauth_provider_configured && (
          <Badge variant="info" size="sm">
            {t("plugins.summary.oauthConfigured")}
          </Badge>
        )}
      </div>
    );
  }
  return (
    <Badge variant={summary.configured ? "success" : "secondary"} size="sm">
      {summary.configured ? t("plugins.summary.configured") : t("plugins.summary.notConfigured")}
      {summary.kind ? ` · ${summary.kind}` : ""}
    </Badge>
  );
}

function ConfigRow({
  config,
  onEnabled,
  onInherit,
  onEdit,
  onReset,
  onDelete,
  onOAuthConnect,
  onOAuthDisconnect,
  busy,
  t,
}: {
  config: PluginConfig;
  onEnabled: (enabled: boolean) => void;
  onInherit: () => void;
  onEdit: () => void;
  onReset?: () => void;
  onDelete?: () => void;
  onOAuthConnect?: () => void;
  onOAuthDisconnect?: () => void;
  busy: boolean;
  t: Translate;
}) {
  const enabledLabel =
    config.is_enabled === null
      ? t("plugins.inherited")
      : config.is_enabled
        ? t("plugins.enabled")
        : t("plugins.disabled");
  return (
    <Card className="gap-3 p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-1">
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-sm font-medium">{scopeLabel(config.scope, t)}</span>
            <Badge
              variant={
                config.is_enabled === null ? "secondary" : config.is_enabled ? "success" : "warning"
              }
              size="sm"
            >
              {enabledLabel}
            </Badge>
          </div>
          <p className="text-xs text-muted-foreground">
            {config.agent_id ? `${t("plugins.agent")}: ${config.agent_id}` : t("plugins.allAgents")}
          </p>
          <BackendSummary config={config} t={t} />
        </div>
        <div className="flex items-center gap-2">
          <Button variant="ghost" size="xs" onClick={onEdit} disabled={busy}>
            {t("common.edit")}
          </Button>
          {onOAuthConnect && (
            <Button variant="outline" size="xs" onClick={onOAuthConnect} disabled={busy}>
              {t("plugins.oauthAuthorize")}
            </Button>
          )}
          {onOAuthDisconnect && (
            <Button variant="ghost" size="xs" onClick={onOAuthDisconnect} disabled={busy}>
              {t("plugins.oauthDisconnect")}
            </Button>
          )}
          <Switch
            checked={config.is_enabled === true}
            disabled={busy}
            onCheckedChange={onEnabled}
            aria-label={enabledLabel}
          />
          <Button
            variant="ghost"
            size="xs"
            onClick={onInherit}
            disabled={busy || config.is_enabled === null}
          >
            {t("plugins.inherit")}
          </Button>
          {onReset && (
            <Button variant="ghost" size="xs" onClick={onReset} disabled={busy}>
              <RotateCcw className="size-3.5" />
              {t("plugins.resetConfig")}
            </Button>
          )}
          {onDelete && (
            <Button variant="ghost" size="xs" onClick={onDelete} disabled={busy}>
              <Trash2 className="size-3.5" />
              {t("common.delete")}
            </Button>
          )}
        </div>
      </div>
    </Card>
  );
}

export function UnifiedPluginsPage({ scopeBand = "system" }: { scopeBand?: ScopeBand }) {
  const { t } = useI18n();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { showToast } = useToast(4000);
  const params = useParams({ strict: false }) as { pluginId?: string };
  const pluginsQuery = useQuery(pluginsQueryOptions);
  const agentsQuery = useQuery(
    scopeBand === "system" ? allAgentsAdminQueryOptions(true) : agentsQueryOptions,
  );
  const agents = agentsQuery.data ?? [];
  const [selectedAgentID, setSelectedAgentID] = useState("");
  const visibleScopes = scopesForBand(scopeBand) as readonly PluginScope[];
  const plugins = pluginsQuery.data ?? [];
  const selectedPlugin = useMemo(
    () => plugins.find((plugin) => routeName(plugin) === params.pluginId),
    [plugins, params.pluginId],
  );
  const selectedPath = selectedPlugin ? routePath(selectedPlugin) : null;
  const closeDetail = () =>
    void navigate({
      to: scopeBand === "system" ? "/admin/integrations/plugins" : "/settings/plugins",
    });
  const configQueries = useQueries({
    queries: selectedPath
      ? visibleScopes.map((scope) =>
          pluginConfigsQueryOptions(
            selectedPath.kind,
            selectedPath.name,
            scope,
            isAgentManagedScope(scope) ? selectedAgentID || undefined : undefined,
          ),
        )
      : [],
  });
  const [pendingDelete, setPendingDelete] = useState<PluginConfig | null>(null);
  const [pendingPluginDelete, setPendingPluginDelete] = useState<PluginDefinition | null>(null);
  const [editingConfig, setEditingConfig] = useState<PluginConfig | null>(null);
  const [newMcpOpen, setNewMcpOpen] = useState(false);
  const [registryOpen, setRegistryOpen] = useState(false);
  const [newMcpName, setNewMcpName] = useState("");
  const [newMcpURL, setNewMcpURL] = useState("");
  const [newMcpNamespace, setNewMcpNamespace] = useState("");
  const [newMcpDescription, setNewMcpDescription] = useState("");
  const [newMcpScope, setNewMcpScope] = useState<PluginScope>(visibleScopes[0]);
  const [newScope, setNewScope] = useState<PluginScope>(visibleScopes[0]);
  const closeNewMcp = () => {
    setNewMcpOpen(false);
    setNewMcpURL("");
  };
  const invalidate = () => {
    if (selectedPath)
      void queryClient.invalidateQueries({
        queryKey: ["plugin-configs", selectedPath.kind, selectedPath.name],
      });
    void queryClient.invalidateQueries({ queryKey: ["plugins"] });
  };
  const configMutation = useMutation({
    mutationFn: async (input: { config: PluginConfig; enabled: boolean | null }) => {
      if (!selectedPath) throw new Error(t("plugins.noSelection"));
      const { data } = await updatePluginConfig({
        path: { ...selectedPath, config_id: input.config.id },
        body: {
          expected_revision: input.config.revision,
          is_enabled: input.enabled,
        },
        throwOnError: true,
      });
      return data;
    },
    onSuccess: () => {
      invalidate();
      showToast(t("plugins.configUpdated"));
    },
    onError: (error) => showToast(pluginErrorMessage(error, t), "error"),
  });
  const resetMutation = useMutation({
    mutationFn: async (config: PluginConfig) => {
      if (!selectedPath) throw new Error(t("plugins.noSelection"));
      const { data } = await resetPluginConfig({
        path: { ...selectedPath, config_id: config.id },
        body: { expected_revision: config.revision },
        throwOnError: true,
      });
      return data;
    },
    onSuccess: () => {
      invalidate();
      showToast(t("plugins.configReset"));
    },
    onError: (error) => showToast(pluginErrorMessage(error, t), "error"),
  });
  const deleteMutation = useMutation({
    mutationFn: async (config: PluginConfig) => {
      if (!selectedPath) throw new Error(t("plugins.noSelection"));
      await deletePluginConfig({
        path: { ...selectedPath, config_id: config.id },
        query: { expected_revision: config.revision },
        throwOnError: true,
      });
    },
    onSuccess: () => {
      invalidate();
      showToast(t("plugins.configDeleted"));
    },
    onError: (error) => showToast(pluginErrorMessage(error, t), "error"),
  });
  const createMutation = useMutation({
    mutationFn: async () => {
      if (!selectedPath) throw new Error(t("plugins.noSelection"));
      const { data } = await createPluginConfig({
        path: selectedPath,
        body: {
          scope: newScope,
          ...(isAgentManagedScope(newScope) && selectedAgentID
            ? { agent_id: selectedAgentID }
            : {}),
          is_enabled: false,
        },
        throwOnError: true,
      });
      return data;
    },
    onSuccess: () => {
      invalidate();
      showToast(t("plugins.configCreated"));
    },
    onError: (error) => showToast(pluginErrorMessage(error, t), "error"),
  });
  const editMutation = useMutation({
    mutationFn: async (input: {
      config: PluginConfig;
      payload: PluginConfigPayload;
      credentials: PluginConfigCredentials;
    }) => {
      if (!selectedPath) throw new Error(t("plugins.noSelection"));
      const body: ComponentsUpdatePluginConfigRequestWritable = {
        expected_revision: input.config.revision,
      };
      if (input.payload.config) body.config = input.payload.config;
      if (input.payload.binary_versions) body.binary_versions = input.payload.binary_versions;
      if (Object.keys(input.credentials).length > 0) body.credentials = input.credentials;
      const { data } = await updatePluginConfig({
        path: { ...selectedPath, config_id: input.config.id },
        body,
        throwOnError: true,
      });
      return data;
    },
    onSuccess: () => {
      invalidate();
      setEditingConfig(null);
      showToast(t("plugins.configUpdated"));
    },
    onError: (error) => showToast(pluginErrorMessage(error, t), "error"),
  });
  const oauthStartMutation = useMutation({
    mutationFn: async (config: PluginConfig) => {
      if (!selectedPath) throw new Error(t("plugins.noSelection"));
      const { data } = await startPluginConfigOAuth({
        path: { ...selectedPath, config_id: config.id },
        throwOnError: true,
      });
      return data;
    },
    onSuccess: (data) => {
      if (data?.authorization_url) window.location.href = data.authorization_url;
    },
    onError: (error) => showToast(pluginErrorMessage(error, t), "error"),
  });
  const oauthDisconnectMutation = useMutation({
    mutationFn: async (config: PluginConfig) => {
      if (!selectedPath) throw new Error(t("plugins.noSelection"));
      await disconnectPluginConfigOAuth({
        path: { ...selectedPath, config_id: config.id },
        throwOnError: true,
      });
    },
    onSuccess: () => {
      invalidate();
      showToast(t("plugins.oauthDisconnected"));
    },
    onError: (error) => showToast(pluginErrorMessage(error, t), "error"),
  });
  const createMcpMutation = useMutation({
    mutationFn: async (input: {
      payload: PluginConfigPayload;
      credentials: PluginConfigCredentials;
    }) => {
      const displayName = newMcpName.trim();
      const namespace = newMcpNamespace.trim();
      if (!displayName || !namespace) throw new Error(t("plugins.mcpIdentityRequired"));
      const initialConfig: ComponentsPluginConfigInputWritable = {
        scope: newMcpScope,
        is_enabled: false,
        config: input.payload.config,
      };
      if (isAgentManagedScope(newMcpScope) && selectedAgentID) {
        initialConfig.agent_id = selectedAgentID;
      }
      if (Object.keys(input.credentials).length > 0) {
        initialConfig.credentials = input.credentials;
      }
      const body: ComponentsCreatePluginRequestWritable = {
        display_name: displayName,
        namespace,
        backend: "mcp",
        definition_spec: newMcpDescription.trim() ? { description: newMcpDescription.trim() } : {},
        initial_config: initialConfig,
      };
      const { data } = await createPlugin({
        body,
        throwOnError: true,
      });
      return data;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["plugins"] });
      closeNewMcp();
      setNewMcpName("");
      setNewMcpNamespace("");
      setNewMcpDescription("");
      showToast(t("plugins.created"));
    },
    onError: (error) => showToast(pluginErrorMessage(error, t), "error"),
  });
  const definitionDeleteMutation = useMutation({
    mutationFn: async (plugin: PluginDefinition) => {
      const path = routePath(plugin);
      await deletePlugin({
        path,
        query: { expected_revision: plugin.revision },
        throwOnError: true,
      });
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["plugins"] });
      showToast(t("plugins.deleted"));
      closeDetail();
    },
    onError: (error) => showToast(pluginErrorMessage(error, t), "error"),
  });
  const groups = useMemo(() => {
    const channel = plugins.filter(
      (plugin) => plugin.backend === "go" && plugin.id.startsWith("channel/"),
    );
    const cli = plugins.filter((plugin) => plugin.backend === "cli");
    const mcp = plugins.filter((plugin) => plugin.backend === "mcp");
    const system = plugins.filter(
      (plugin) => !channel.includes(plugin) && !cli.includes(plugin) && !mcp.includes(plugin),
    );
    return [
      { title: t("plugins.group.channels"), items: channel },
      { title: t("plugins.group.cli"), items: cli },
      { title: t("plugins.group.mcp"), items: mcp },
      { title: t("plugins.group.system"), items: system },
    ];
  }, [plugins, t]);
  if (pluginsQuery.isPending)
    return (
      <div className="flex h-full items-center justify-center">
        <Spinner className="size-5 text-muted-foreground" />
      </div>
    );
  if (pluginsQuery.isError)
    return (
      <ErrorState
        title={t("plugins.loadFailed")}
        description={pluginErrorMessage(pluginsQuery.error, t)}
        onRetry={() => void pluginsQuery.refetch()}
      />
    );
  const detail = selectedPlugin ? (
    <DetailPanel>
      <DetailPanelHeader
        title={selectedPlugin.display_name}
        subtitle={
          <div className="flex flex-wrap items-center gap-1.5">
            <Badge variant="outline" size="sm">
              {backendTitle(selectedPlugin, t)}
            </Badge>
            {selectedPlugin.is_builtin && (
              <Badge variant="secondary" size="sm">
                {t("plugins.builtin")}
              </Badge>
            )}
          </div>
        }
      />
      {typeof selectedPlugin.spec.description === "string" && selectedPlugin.spec.description && (
        <p className="text-sm text-muted-foreground">{selectedPlugin.spec.description}</p>
      )}
      <Field>
        <FieldLabel>{t("plugins.agent")}</FieldLabel>
        <Select
          value={selectedAgentID || "__none"}
          onValueChange={(value) => setSelectedAgentID(value === "__none" || !value ? "" : value)}
        >
          <SelectTrigger>
            <SelectValue placeholder={t("plugins.selectAgent")} />
          </SelectTrigger>
          <SelectPopup>
            <SelectItem value="__none">{t("plugins.selectAgent")}</SelectItem>
            {agents.map((agent) => (
              <SelectItem key={agent.id} value={agent.id}>
                {agent.name}
              </SelectItem>
            ))}
          </SelectPopup>
        </Select>
      </Field>
      <div className="space-y-2">
        <p className="text-xs font-semibold text-muted-foreground">{t("plugins.configuration")}</p>
        {visibleScopes.map((scope, index) => {
          const query = configQueries[index];
          const configs = query.data ?? [];
          return (
            <section key={scope} className="space-y-2">
              <div className="flex items-center justify-between gap-2">
                <div className="flex items-center gap-2">
                  <h3 className="text-sm font-medium">{scopeLabel(scope, t)}</h3>
                  <Badge variant="secondary" size="sm">
                    {configs.length}
                  </Badge>
                </div>
                {query.isFetching && <Spinner className="size-4 text-muted-foreground" />}
              </div>
              {isAgentManagedScope(scope) && !selectedAgentID ? (
                <p className="text-xs text-muted-foreground">{t("plugins.selectAgent")}</p>
              ) : query.isError ? (
                <p className="text-xs text-destructive-foreground">
                  {t("plugins.scopeUnavailable")}
                </p>
              ) : configs.length === 0 ? (
                <p className="text-xs text-muted-foreground">{t("plugins.noScopeConfig")}</p>
              ) : (
                configs.map((config) => (
                  <ConfigRow
                    key={config.id}
                    config={config}
                    busy={
                      configMutation.isPending ||
                      resetMutation.isPending ||
                      deleteMutation.isPending ||
                      oauthStartMutation.isPending ||
                      oauthDisconnectMutation.isPending
                    }
                    onEnabled={(enabled) => configMutation.mutate({ config, enabled })}
                    onInherit={() => configMutation.mutate({ config, enabled: null })}
                    onEdit={() => setEditingConfig(config)}
                    onReset={
                      selectedPlugin.is_builtin && scope === "system"
                        ? () => resetMutation.mutate(config)
                        : undefined
                    }
                    onDelete={
                      selectedPlugin.is_builtin && scope === "system"
                        ? undefined
                        : () => setPendingDelete(config)
                    }
                    onOAuthConnect={
                      selectedPlugin.backend === "mcp" &&
                      config.backend_summary.backend === "mcp" &&
                      config.backend_summary.auth_type === "oauth"
                        ? () => oauthStartMutation.mutate(config)
                        : undefined
                    }
                    onOAuthDisconnect={
                      selectedPlugin.backend === "mcp" &&
                      config.backend_summary.backend === "mcp" &&
                      config.backend_summary.auth_type === "oauth"
                        ? () => oauthDisconnectMutation.mutate(config)
                        : undefined
                    }
                    t={t}
                  />
                ))
              )}
            </section>
          );
        })}
      </div>
      <div className="space-y-3 border-t border-border pt-4">
        <p className="text-xs font-semibold text-muted-foreground">{t("plugins.addScopeConfig")}</p>
        <div className="grid gap-3 sm:grid-cols-2">
          <Field>
            <FieldLabel>{t("plugins.scopeLabel")}</FieldLabel>
            <Select value={newScope} onValueChange={(value) => setNewScope(value as PluginScope)}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectPopup>
                {visibleScopes.map((scope) => (
                  <SelectItem key={scope} value={scope}>
                    {scopeLabel(scope, t)}
                  </SelectItem>
                ))}
              </SelectPopup>
            </Select>
          </Field>
          {(newScope === "system_agent" || newScope === "user_agent") && (
            <Field>
              <FieldLabel>{t("plugins.agent")}</FieldLabel>
              <Select
                value={selectedAgentID || "__none"}
                onValueChange={(value) =>
                  setSelectedAgentID(value === "__none" || !value ? "" : value)
                }
              >
                <SelectTrigger>
                  <SelectValue placeholder={t("plugins.selectAgent")} />
                </SelectTrigger>
                <SelectPopup>
                  <SelectItem value="__none">{t("plugins.selectAgent")}</SelectItem>
                  {agents.map((agent) => (
                    <SelectItem key={agent.id} value={agent.id}>
                      {agent.name}
                    </SelectItem>
                  ))}
                </SelectPopup>
              </Select>
            </Field>
          )}
        </div>
        <Button
          variant="outline"
          size="sm"
          loading={createMutation.isPending}
          disabled={(newScope === "system_agent" || newScope === "user_agent") && !selectedAgentID}
          onClick={() => createMutation.mutate()}
        >
          {t("plugins.addScopeConfig")}
        </Button>
      </div>
      {!selectedPlugin.is_builtin && (
        <div className="border-t border-border pt-4">
          <Button
            variant="destructive"
            size="sm"
            loading={definitionDeleteMutation.isPending}
            onClick={() => setPendingPluginDelete(selectedPlugin)}
          >
            <Trash2 className="size-3.5" />
            {t("common.delete")}
          </Button>
        </div>
      )}
    </DetailPanel>
  ) : null;
  const newMcpPlugin = {
    backend: "mcp" as const,
    display_name: newMcpName || t("plugins.newMcp"),
  };
  return (
    <>
      <SettingsGridPage
        title={t("plugins.title")}
        action={
          <div className="flex gap-2">
            <Button size="sm" variant="outline" onClick={() => setRegistryOpen(true)}>
              {t("mcp.market.title")}
            </Button>
            <Button size="sm" variant="outline" onClick={() => setNewMcpOpen(true)}>
              <Plus className="size-3.5" />
              {t("plugins.addMcp")}
            </Button>
          </div>
        }
      >
        {plugins.length === 0 ? (
          <ErrorState title={t("plugins.noPlugins")} description={t("plugins.noPluginsDesc")} />
        ) : (
          groups.map(
            (group) =>
              group.items.length > 0 && (
                <SettingsCardSection
                  key={group.title}
                  title={group.title}
                  count={group.items.length}
                >
                  {group.items.map((plugin) => (
                    <Card
                      key={plugin.id}
                      render={
                        <Link
                          to={
                            scopeBand === "system"
                              ? "/admin/integrations/plugins/$pluginId"
                              : "/settings/plugins/$pluginId"
                          }
                          params={{ pluginId: routeName(plugin) }}
                        />
                      }
                      className="gap-3 p-4 transition-colors hover:border-ring/40"
                    >
                      <div className="flex items-start gap-3">
                        <span className="grid size-9 shrink-0 place-items-center rounded-lg border border-border bg-muted text-muted-foreground">
                          {backendIcon(plugin)}
                        </span>
                        <div className="min-w-0 flex-1 space-y-1">
                          <div className="flex flex-wrap items-center gap-1.5">
                            <span className="truncate text-sm font-medium">
                              {plugin.display_name}
                            </span>
                            {plugin.is_builtin && (
                              <Badge variant="secondary" size="sm">
                                {t("plugins.builtin")}
                              </Badge>
                            )}
                          </div>
                          <p className="text-xs text-muted-foreground">
                            {typeof plugin.spec.description === "string" && plugin.spec.description
                              ? plugin.spec.description
                              : t("plugins.noDescription")}
                          </p>
                        </div>
                      </div>
                    </Card>
                  ))}
                </SettingsCardSection>
              ),
          )
        )}
      </SettingsGridPage>
      <McpInstallSheet
        open={registryOpen}
        onOpenChange={setRegistryOpen}
        notify={showToast}
        defaultScope={scopeBand === "system" ? "system" : "user"}
        isAdmin={scopeBand === "system"}
        agentId={selectedAgentID || undefined}
        onRequestManual={({ name, namespace, url }) => {
          setNewMcpName(name);
          setNewMcpNamespace(namespace);
          setNewMcpURL(url);
          setNewMcpOpen(true);
        }}
      />
      <SettingsDetailSheet open={detail !== null} onClose={closeDetail}>
        {detail}
      </SettingsDetailSheet>
      <SettingsDetailSheet open={newMcpOpen} onClose={closeNewMcp}>
        <DetailPanel>
          <DetailPanelHeader title={t("plugins.addMcp")} />
          <div className="space-y-4">
            <Field>
              <FieldLabel>{t("plugins.mcpName")}</FieldLabel>
              <Input
                value={newMcpName}
                onChange={(event) => setNewMcpName(event.target.value)}
                placeholder={t("plugins.mcpNamePlaceholder")}
                nativeInput
              />
            </Field>
            <Field>
              <FieldLabel>{t("plugins.mcpNamespace")}</FieldLabel>
              <Input
                value={newMcpNamespace}
                onChange={(event) => setNewMcpNamespace(event.target.value)}
                placeholder={t("plugins.mcpNamespacePlaceholder")}
                nativeInput
              />
            </Field>
            <Field>
              <FieldLabel>{t("plugins.mcpDescription")}</FieldLabel>
              <Input
                value={newMcpDescription}
                onChange={(event) => setNewMcpDescription(event.target.value)}
                placeholder={t("plugins.mcpDescriptionPlaceholder")}
                nativeInput
              />
            </Field>
            <Field>
              <FieldLabel>{t("plugins.scopeLabel")}</FieldLabel>
              <Select
                value={newMcpScope}
                onValueChange={(value) => setNewMcpScope(value as PluginScope)}
              >
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectPopup>
                  {visibleScopes.map((scope) => (
                    <SelectItem key={scope} value={scope}>
                      {scopeLabel(scope, t)}
                    </SelectItem>
                  ))}
                </SelectPopup>
              </Select>
            </Field>
            {(newMcpScope === "system_agent" || newMcpScope === "user_agent") && (
              <Field>
                <FieldLabel>{t("plugins.agent")}</FieldLabel>
                <Select
                  value={selectedAgentID || "__none"}
                  onValueChange={(value) =>
                    setSelectedAgentID(value === "__none" || !value ? "" : value)
                  }
                >
                  <SelectTrigger>
                    <SelectValue placeholder={t("plugins.selectAgent")} />
                  </SelectTrigger>
                  <SelectPopup>
                    <SelectItem value="__none">{t("plugins.selectAgent")}</SelectItem>
                    {agents.map((agent) => (
                      <SelectItem key={agent.id} value={agent.id}>
                        {agent.name}
                      </SelectItem>
                    ))}
                  </SelectPopup>
                </Select>
              </Field>
            )}
            <PluginConfigEditor
              plugin={newMcpPlugin}
              initialMcpUrl={newMcpURL}
              onSave={(payload, credentials) => {
                const url = payload.config?.url;
                if (typeof url !== "string" || !url.trim()) {
                  showToast(t("plugins.mcpEndpointRequired"), "error");
                  return;
                }
                createMcpMutation.mutate({ payload, credentials });
              }}
              onCancel={closeNewMcp}
              busy={createMcpMutation.isPending}
            />
          </div>
        </DetailPanel>
      </SettingsDetailSheet>
      <SettingsDetailSheet open={editingConfig !== null} onClose={() => setEditingConfig(null)}>
        {editingConfig && selectedPlugin && (
          <DetailPanel>
            <DetailPanelHeader
              title={t("plugins.editConfiguration")}
              subtitle={scopeLabel(editingConfig.scope, t)}
            />
            <PluginConfigEditor
              plugin={selectedPlugin}
              config={editingConfig}
              onSave={(payload, credentials) => {
                editMutation.mutate({ config: editingConfig, payload, credentials });
              }}
              onCancel={() => setEditingConfig(null)}
              busy={editMutation.isPending}
            />
          </DetailPanel>
        )}
      </SettingsDetailSheet>
      <ConfirmDialog
        open={pendingDelete !== null}
        onOpenChange={(open) => !open && setPendingDelete(null)}
        title={t("plugins.deleteConfig")}
        message={
          pendingDelete
            ? t("plugins.deleteConfigMsg", {
                scope: scopeLabel(pendingDelete.scope, t),
              })
            : ""
        }
        onConfirm={() => {
          if (pendingDelete) deleteMutation.mutate(pendingDelete);
          setPendingDelete(null);
        }}
      />
      <ConfirmDialog
        open={pendingPluginDelete !== null}
        onOpenChange={(open) => !open && setPendingPluginDelete(null)}
        title={t("plugins.deleteConfirm")}
        message={
          pendingPluginDelete
            ? t("plugins.deleteConfirmDesc", {
                name: pendingPluginDelete.display_name,
              })
            : ""
        }
        onConfirm={() => {
          if (pendingPluginDelete) definitionDeleteMutation.mutate(pendingPluginDelete);
          setPendingPluginDelete(null);
        }}
      />
    </>
  );
}

export function PersonalUnifiedPluginsPage() {
  return <UnifiedPluginsPage scopeBand="personal" />;
}
