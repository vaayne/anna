import { useMutation, useQueryClient } from "@tanstack/react-query";
import { RefreshCw } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Drawer,
  DrawerClose,
  DrawerHeader,
  DrawerPopup,
  DrawerTitle,
} from "@/components/ui/drawer";
import { probePluginConfig } from "@/lib/api-client/sdk.gen";
import type { AgentMcpServer } from "@/lib/api-client/types.gen";
import { apiErrorMessage } from "@/lib/api-error";
import { useI18n } from "@/lib/i18n";
import { SCOPE_LABEL_KEY } from "@/lib/skill-scope";

function pluginPath(pluginID: string) {
  const slash = pluginID.indexOf("/");
  if (slash <= 0 || slash === pluginID.length - 1) throw new Error("invalid plugin id");
  return { kind: pluginID.slice(0, slash), name: pluginID.slice(slash + 1) };
}

function statusBadgeVariant(status: string) {
  if (status === "ok") return "success";
  if (status === "error" || status === "needs_auth") return "warning";
  return "outline";
}

export function McpServerDrawer({
  server,
  open,
  onOpenChange,
  onConnect,
  onDisconnect,
  onEdit,
  onDelete,
  notify,
}: {
  server: AgentMcpServer | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onConnect: (server: AgentMcpServer) => void;
  onDisconnect: (server: AgentMcpServer) => void;
  onEdit: (server: AgentMcpServer) => void;
  onDelete: (server: AgentMcpServer) => void;
  notify: (message: string, kind?: "success" | "error") => void;
}) {
  const { t } = useI18n();
  const queryClient = useQueryClient();
  const probe = useMutation({
    mutationFn: (target: AgentMcpServer) =>
      probePluginConfig({
        path: { ...pluginPath(target.plugin_id), config_id: target.config_id },
        throwOnError: true,
      }),
    onSuccess: async () => {
      notify(t("mcp.server.probed", { time: new Date().toISOString() }), "success");
      await queryClient.invalidateQueries({ queryKey: ["agent-mcp-servers"] });
    },
    onError: (error) => notify(apiErrorMessage(error, t("mcp.saveFailed")), "error"),
  });
  if (!server) return null;
  return (
    <Drawer open={open} onOpenChange={onOpenChange} position="right">
      <DrawerPopup position="right" className="w-full sm:w-[480px] sm:max-w-[480px]">
        <DrawerHeader>
          <DrawerTitle className="min-w-0 truncate font-mono">{server.namespace}</DrawerTitle>
          <DrawerClose aria-label={t("common.close")} />
        </DrawerHeader>
        <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-5">
          <div className="flex flex-wrap items-center gap-2">
            <Badge variant="outline" size="sm">
              {t(SCOPE_LABEL_KEY[server.scope])}
            </Badge>
            <Badge variant={statusBadgeVariant(server.status)} size="sm">
              {t(`mcp.status.${server.status}` as never)}
            </Badge>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Button
              variant="outline"
              size="sm"
              loading={probe.isPending}
              onClick={() => probe.mutate(server)}
            >
              <RefreshCw size={16} />
              {t("mcp.server.probe")}
            </Button>
            {server.credential_mode === "per_user" && (
              <Button
                variant="outline"
                size="sm"
                onClick={() => (server.needs_auth ? onConnect(server) : onDisconnect(server))}
              >
                {server.needs_auth ? t("mcp.connect") : t("mcp.disconnect")}
              </Button>
            )}
            {server.readable && (
              <>
                <Button variant="ghost" size="sm" onClick={() => onEdit(server)}>
                  {t("common.edit")}
                </Button>
                <Button variant="ghost" size="sm" onClick={() => onDelete(server)}>
                  {t("common.delete")}
                </Button>
              </>
            )}
          </div>
          <div className="space-y-2">
            <p className="text-xs font-medium text-muted-foreground">{t("mcp.server.tools")}</p>
            {server.tools.length === 0 ? (
              <p className="text-sm text-muted-foreground">{t("mcp.server.noTools")}</p>
            ) : (
              server.tools.map((tool) => (
                <div key={tool.name} className="rounded-lg border p-3">
                  <p className="truncate font-mono text-sm font-medium">{tool.name}</p>
                  {tool.description && (
                    <p className="mt-1 text-xs text-muted-foreground">{tool.description}</p>
                  )}
                </div>
              ))
            )}
          </div>
        </div>
      </DrawerPopup>
    </Drawer>
  );
}
