import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { Sheet, SheetPopup } from "@/components/ui/sheet";
import { DetailPanel, DetailPanelHeader } from "@/features/settings/SettingsDetailPanel";
import {
  PluginConfigEditor,
  type PluginConfigPayload,
} from "@/features/plugins/PluginConfigEditor";
import { McpInstallSheet } from "@/features/mcp/McpInstallSheet";
import { getPlugin, getPluginConfig, updatePluginConfig } from "@/lib/api-client/sdk.gen";
import type { AgentMcpServer, PluginConfig, PluginDefinition } from "@/lib/api-client/types.gen";
import { apiErrorMessage } from "@/lib/api-error";
import { useI18n } from "@/lib/i18n";

type Notify = (message: string, kind?: "success" | "error") => void;

function pluginPath(pluginID: string) {
  const slash = pluginID.indexOf("/");
  if (slash <= 0 || slash === pluginID.length - 1) throw new Error("invalid plugin id");
  return { kind: pluginID.slice(0, slash), name: pluginID.slice(slash + 1) };
}

export function AgentMcpServerSheet({
  agentId,
  isAdmin,
  open,
  server,
  formKey,
  onOpenChange,
  notify,
}: {
  agentId: string;
  isAdmin: boolean;
  open: boolean;
  server: AgentMcpServer | null;
  formKey: number;
  onOpenChange: (open: boolean) => void;
  notify: Notify;
}) {
  const { t } = useI18n();
  const queryClient = useQueryClient();
  const path = server ? pluginPath(server.plugin_id) : null;
  const configQuery = useQuery({
    queryKey: ["plugin-config", path?.kind, path?.name, server?.config_id],
    enabled: open && !!path && !!server?.readable,
    queryFn: async () => {
      const { data } = await getPluginConfig({
        path: { ...path!, config_id: server!.config_id },
        throwOnError: true,
      });
      return data as PluginConfig;
    },
  });
  const pluginQuery = useQuery({
    queryKey: ["plugin", path?.kind, path?.name],
    enabled: open && !!path,
    queryFn: async () => {
      const { data } = await getPlugin({ path: path!, throwOnError: true });
      return data as PluginDefinition;
    },
  });
  const updateMutation = useMutation({
    mutationFn: async ({
      config,
      payload,
    }: {
      config: PluginConfig;
      payload: PluginConfigPayload;
    }) => {
      const { data } = await updatePluginConfig({
        path: { ...path!, config_id: config.id },
        body: {
          expected_revision: config.revision,
          ...(payload.config ? { config: payload.config } : {}),
          ...(payload.binary_versions ? { binary_versions: payload.binary_versions } : {}),
        },
        throwOnError: true,
      });
      return data;
    },
    onSuccess: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["agent-tools", agentId] }),
        queryClient.invalidateQueries({ queryKey: ["agent-mcp-servers", agentId] }),
      ]);
      notify(t("mcp.updated"), "success");
      onOpenChange(false);
    },
    onError: (error) => notify(apiErrorMessage(error, t("mcp.saveFailed")), "error"),
  });

  if (!server) {
    return (
      <McpInstallSheet
        open={open}
        onOpenChange={onOpenChange}
        notify={notify}
        defaultScope="user_agent"
        agentId={agentId}
        isAdmin={isAdmin}
        key={formKey}
      />
    );
  }

  const config = configQuery.data;
  const plugin = pluginQuery.data;
  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetPopup side="right" className="w-full sm:w-[560px] sm:max-w-[560px]">
        <DetailPanel>
          <DetailPanelHeader title={plugin?.display_name ?? server.namespace} />
          {config && plugin ? (
            <PluginConfigEditor
              plugin={plugin}
              config={config}
              onSave={(payload, credentials) => {
                if (Object.keys(credentials).length > 0) {
                  notify(t("plugins.secretWriteUnavailable"), "error");
                  return;
                }
                updateMutation.mutate({ config, payload });
              }}
              onCancel={() => onOpenChange(false)}
              busy={updateMutation.isPending}
            />
          ) : (
            <div className="space-y-3">
              <p className="text-sm text-muted-foreground">
                {configQuery.isError || pluginQuery.isError
                  ? t("plugins.scopeUnavailable")
                  : t("agents.tools.loading")}
              </p>
              <Button variant="ghost" size="sm" onClick={() => onOpenChange(false)}>
                {t("common.cancel")}
              </Button>
            </div>
          )}
        </DetailPanel>
      </SheetPopup>
    </Sheet>
  );
}
