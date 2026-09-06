import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { createPlugin, probePluginConfig, startPluginConfigOAuth } from "@/lib/api-client/sdk.gen";
import type {
  ComponentsPluginConfigInputWritable,
  McpRegistryServer,
  PluginConfig,
} from "@/lib/api-client/types.gen";
import type { InstallRequest, WritableScope } from "@/features/marketplace/InstallScopeStep";
import { apiErrorMessage } from "@/lib/api-error";

// One marketplace install creates a first-party MCP plugin and its initial
// config. Credentials remain write-only and are handed to the common backend
// mutation seam, never echoed through the returned config summary.
export type InstallArgs = {
  server: McpRegistryServer;
  scope: WritableScope;
  agentId?: string;
  bearerSecret?: string;
};

/** Registry IDs can contain vendor path punctuation; plugin namespaces cannot. */
export function registryPluginNamespace(id: string): string {
  const namespace = id.replace(/[^A-Za-z0-9-]+/g, "-").replace(/^-+|-+$/g, "");
  if (!namespace) throw new Error("registry server id cannot produce a valid plugin namespace");
  return namespace;
}

// The notify/t callbacks come from the host sheet; call sites only pass
// MessageKey literals, so the translator type is imported rather than re-derived.
import type { useI18n } from "@/lib/i18n";

export function useMcpMarketInstall(
  notify: (message: string, kind?: "success" | "error") => void,
  t: ReturnType<typeof useI18n>["t"],
) {
  const queryClient = useQueryClient();
  const [created, setCreated] = useState<PluginConfig | null>(null);

  const mutation = useMutation({
    mutationFn: async ({ server, scope, agentId, bearerSecret }: InstallArgs) => {
      const authType = server.auth === "bearer" ? "bearer" : "none";
      const registry = server.version
        ? { source: server.source, id: server.id, version: server.version }
        : { source: server.source, id: server.id };
      const config = {
        url: server.url,
        transport: server.transport,
        auth_type: authType,
        credential_mode: "shared" as const,
        metadata: { registry },
      };
      const initialConfig: ComponentsPluginConfigInputWritable = {
        scope,
        is_enabled: true,
        config,
      };
      if (agentId) initialConfig.agent_id = agentId;
      if (authType === "bearer") {
        const token = bearerSecret?.trim();
        if (!token) throw new Error("bearer credential is required");
        initialConfig.credentials = { token };
      }
      const { data } = await createPlugin({
        body: {
          display_name: server.name,
          namespace: registryPluginNamespace(server.id),
          backend: "mcp",
          definition_spec: {},
          initial_config: {
            ...initialConfig,
          },
        },
        throwOnError: true,
      });
      const createdConfig = data?.config;
      if (!createdConfig) throw new Error("plugin configuration was not returned");
      const [kind, ...nameParts] = createdConfig.plugin_id.split("/");
      if (!kind || nameParts.length === 0) throw new Error("invalid plugin id");
      await probePluginConfig({
        path: { kind, name: nameParts.join("/"), config_id: createdConfig.id },
        throwOnError: true,
      });
      return createdConfig;
    },
    onSuccess: (data) => {
      setCreated(data ?? null);
      void queryClient.invalidateQueries({ queryKey: ["plugins"] });
      void queryClient.invalidateQueries({ queryKey: ["plugin-configs"] });
      void queryClient.invalidateQueries({ queryKey: ["agent-mcp-servers"] });
      void queryClient.invalidateQueries({ queryKey: ["mcp-servers"] });
    },
    onError: (e) => notify(apiErrorMessage(e, t("mcp.saveFailed")), "error"),
  });

  // Nested OAuth is the only connection action. It is intentionally separate
  // from config writes so the callback can enforce common plugin visibility.
  const connect = useMutation({
    mutationFn: async (config: PluginConfig) => {
      const [kind, ...nameParts] = config.plugin_id.split("/");
      if (!kind || nameParts.length === 0) throw new Error("invalid plugin id");
      const { data } = await startPluginConfigOAuth({
        path: { kind, name: nameParts.join("/"), config_id: config.id },
        throwOnError: true,
      });
      return data?.authorization_url ?? "";
    },
    onSuccess: (url) => {
      if (url) window.location.href = url;
    },
    onError: (e) => notify(apiErrorMessage(e, t("mcp.connectFailed")), "error"),
  });

  return { mutation, created, setCreated, connect, connectPending: connect.isPending };
}

/** Builds the deferred install request handed to the shared scope step. */
export function buildInstallRequest(
  server: McpRegistryServer,
  run: (args: InstallArgs) => Promise<PluginConfig>,
  confirmLabel: string,
  agentId?: string,
  bearerSecret?: string,
): InstallRequest<WritableScope> {
  return {
    name: server.name,
    confirmLabel,
    run: async (scope) => {
      // The mutation reports its own failure; a rejected run keeps the step open.
      try {
        await run({ server, scope, agentId, bearerSecret });
        return true;
      } catch {
        return false;
      }
    },
  };
}
