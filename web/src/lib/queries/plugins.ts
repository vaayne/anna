import { queryOptions } from "@tanstack/react-query";
import { listPluginConfigs, listPlugins } from "@/lib/api-client";
import type { PluginConfig, PluginDefinition } from "@/lib/api-client";

export type PluginScope = PluginConfig["scope"];

async function fetchAllPlugins(): Promise<PluginDefinition[]> {
  const plugins: PluginDefinition[] = [];
  let pageToken: string | undefined;
  do {
    const { data } = await listPlugins({
      query: {
        page_size: 500,
        ...(pageToken ? { page_token: pageToken } : {}),
      },
      throwOnError: true,
    });
    plugins.push(...(data?.plugins ?? []));
    pageToken = data?.next_page_token ?? undefined;
  } while (pageToken);
  return plugins;
}

export const pluginsQueryOptions = queryOptions({
  queryKey: ["plugins"],
  queryFn: fetchAllPlugins,
});

export const pluginConfigsQueryOptions = (
  kind: string,
  name: string,
  scope: PluginScope,
  agentID?: string,
) =>
  queryOptions({
    queryKey: ["plugin-configs", kind, name, scope, agentID ?? null],
    // Agent-owned scopes require an explicit PEP target. A disabled query is
    // preferable to a broad request that could accidentally enumerate agents.
    enabled: scope === "user" || scope === "system" || !!agentID,
    queryFn: async () => {
      const configs: PluginConfig[] = [];
      let pageToken: string | undefined;
      do {
        const { data } = await listPluginConfigs({
          path: { kind, name },
          query: {
            scope,
            ...(agentID ? { agent_id: agentID } : {}),
            page_size: 500,
            ...(pageToken ? { page_token: pageToken } : {}),
          },
          throwOnError: true,
        });
        configs.push(...(data?.configs ?? []));
        pageToken = data?.next_page_token ?? undefined;
      } while (pageToken);
      return configs;
    },
  });
