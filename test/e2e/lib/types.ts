export type PluginScope = "system" | "system_agent" | "user" | "user_agent";

export interface PluginDefinition {
  id: string;
  namespace: string;
  display_name: string;
  backend: "cli" | "mcp" | "go";
  is_builtin: boolean;
  is_default_enabled: boolean;
  spec: Record<string, unknown>;
  revision: number;
  created_at: string;
  updated_at: string;
}

export interface PluginMCPBackendSummary {
  backend: "mcp";
  transport: "streamable_http" | "sse";
  auth_type: "none" | "bearer" | "oauth";
  credential_mode: "shared" | "per_user";
  endpoint_configured: boolean;
  bearer_configured: boolean;
  oauth_client_id_configured: boolean;
  oauth_client_secret_configured: boolean;
}

export interface PluginConfig {
  id: string;
  plugin_id: string;
  scope: PluginScope;
  user_id?: string;
  agent_id?: string;
  is_enabled: boolean | null;
  backend_summary: PluginMCPBackendSummary;
  revision: number;
  created_at: string;
  updated_at: string;
}

export interface CreatePluginResponse {
  plugin: PluginDefinition;
  config: PluginConfig;
}
export interface AgentTool {
  name: string;
  enabled: boolean;
  scope: string;
  description?: string;
  [key: string]: unknown;
}
export interface RegistryServer {
  source: string;
  id: string;
  name: string;
  url: string;
  transport: string;
  auth: string;
  version?: string;
  headers?: { name: string; template?: string; }[];
  [key: string]: unknown;
}
