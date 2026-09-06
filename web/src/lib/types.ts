import type {
  ChannelIdentity as SdkChannelIdentity,
  ComponentsAgent,
  ComponentsAgentTool,
  ComponentsAuthUser,
  ComponentsBuiltinResourceDetail,
  ComponentsChannel,
  ComponentsIdentity,
  ComponentsJob,
  ComponentsOAuthFlowStatus,
  ComponentsOAuthProviderConfig,
  ComponentsOAuthProviderStatus,
  ComponentsProvider,
  ComponentsProviderModelItem,
  ComponentsProviderType,
  SessionMessage as SdkSessionMessage,
  ComponentsSkill,
  ComponentsSkillSearchResult,
  ComponentsUserMemory,
  ComponentsVaultEntry,
  JobRun,
  Project as SdkProject,
  SessionWorkspace,
} from "@/lib/api-client/types.gen";

// ── SDK re-exports ────────────────────────────────────────────────────────────
// Fields marked optional in OpenAPI (for create/update) but always present in
// GET responses are overridden to required via intersection.

export type Agent = ComponentsAgent & { id: string; name: string };
export type Session = import("@/lib/api-client/types.gen").ComponentsSession;
export type SessionDetail = import("@/lib/api-client/types.gen").ComponentsSessionDetail;
export type Identity = ComponentsIdentity & { id: string };
export type ChannelIdentity = SdkChannelIdentity & { id: string };
export type Skill = ComponentsSkill & {
  id: string;
  scope: string;
  name: string;
  description: string;
  status: string;
  disable_model_invocation: boolean;
};
export type Tool = ComponentsAgentTool;
export type SchedulerJob = ComponentsJob;
export type SchedulerJobRun = JobRun;
export type BuiltinItem = ComponentsBuiltinResourceDetail;
export type SkillSearchResult = ComponentsSkillSearchResult & {
  description?: string;
};
export type VaultEntry = ComponentsVaultEntry;
export type OAuthProviderConfig = ComponentsOAuthProviderConfig;
export type ProviderType = ComponentsProviderType;
export type Provider = ComponentsProvider;
export type ProviderModel = ComponentsProviderModelItem;
export type Workspace = SessionWorkspace;
export type SessionMessage = SdkSessionMessage;
export const SESSION_MESSAGE_ACTOR_TYPE = {
  human: "human",
  agent: "agent",
  system: "system",
} as const;
export type Project = SdkProject;
export type OAuthFlow = ComponentsOAuthFlowStatus;

// ── SDK extensions (SDK type + UI-only fields) ────────────────────────────────

export type AgentDetail = ComponentsAgent & {
  id: string;
  name: string;
  sandbox: AgentSandbox;
  template_id?: string;
  _highlight?: boolean;
};

export type Channel = ComponentsChannel & {
  _config?: JsonObject;
};

export type User = ComponentsAuthUser & {
  notify_identity_id?: string | null;
  default_agent_id?: string;
};

export type OAuthProvider = ComponentsOAuthProviderStatus & {
  icon?: string;
};

// ── Local types (no SDK equivalent) ───────────────────────────────────────────

export type JsonPrimitive = string | number | boolean | null;
export type JsonValue = JsonPrimitive | JsonValue[] | { [key: string]: JsonValue };
export type JsonObject = { [key: string]: JsonValue };

export interface ToolBlock {
  type: "tool_call";
  id: string;
  name?: string;
  arguments: JsonObject;
  status?: "running" | "done";
  result?: ToolResult;
}

export interface TextBlock {
  type: "text";
  text: string;
}

export interface ImageBlock {
  type: "image";
  media_id: string;
  mime_type: string;
  url: string;
}

export interface ThinkingBlock {
  type: "thinking";
  thinking?: string;
  redacted?: boolean;
}

export type ContentBlock = TextBlock | ImageBlock | ThinkingBlock | ToolBlock;

/**
 * A renderable reference an agent emitted when it created (or referenced) a
 * Stella entity via a CLI tool. `type` + `id` are load-bearing; `preview` is a
 * loading placeholder only — cards hydrate the live entity by id and never
 * trust it past first paint.
 */
export interface RenderableReference {
  v: 1;
  type: "task" | "goal" | "recally_article" | (string & {});
  id: string;
  agent_id?: string;
  intent?: "created" | "referenced";
  preview?: { title?: string; status?: string };
}

export interface ToolResult {
  tool_call_id: string;
  content: string;
  is_error: boolean;
  blocks?: Array<TextBlock | ImageBlock>;
  references?: RenderableReference[];
}

export interface Message {
  id?: string;
  role: "user" | "assistant" | "tool";
  content?: string;
  blocks?: ContentBlock[];
  tool_call_id?: string;
  tool_name?: string;
  is_error?: boolean;
  references?: RenderableReference[];
  timestamp: string;
  token_count?: number;
  model?: string;
  streaming?: boolean;
  actor_type?: "human" | "agent" | "system";
  actor_id?: string;
  source_session_id?: string;
}

export interface AgentSandbox {
  network: {
    mode: "disabled" | "allow_all" | "whitelist";
    allowlist: string[];
  };
}

export type UserMemory = ComponentsUserMemory & {
  agent_id: string;
  content: string;
  soul: string;
  soul_source: "user" | "agent" | "builtin";
  version: number;
  constraints: string;
  /** Absent until the user's first memory write for this agent. */
  updated_at?: string;
};

export interface Personalisation {
  soul: string;
  soulDraft: string;
  profile: string;
  profileDraft: string;
  loaded: boolean;
}
