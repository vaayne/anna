---
title: Architecture
---

> This section is for developers contributing to Stella.

## System Overview

stella is structured as a set of loosely coupled packages wired together at startup. The system supports multiple users and multiple agents, with routing handled per message. The core flow:

1. The **Web UI or a channel** (Telegram, Discord, QQ, Feishu, or WeChat) receives user input.
2. The channel **resolves the user** (upsert by external ID + platform) and **resolves the agent** (DM default, group binding, or fallback).
3. The **ServiceManager** looks up the agent's `agent.Service` by agent ID.
4. `agent.Service` resolves session intent through `session.Registry`.
5. `runtime.Runtime` executes the turn through a cached **Runner**.
6. The **Runner** calls LLM providers and executes tools in a loop.
7. Responses stream back through the channel to the user.

```
Web UI / Channel (Telegram / Discord / QQ / Feishu / WeChat)
    |
    v
Resolve user  -->  Resolve agent
    |
    v
ServiceManager.GetService(agentID)  -->  agent.Service
    |                                      |
    |                                      +--> session.Registry
    |                                      |
    |                                      +--> runtime.Runtime --> Runner
    |                                                             |
    v                                                             v
Channel response stream                                      LLM Provider
```

Session keys are scoped per agent: `{agentID}:{platform}:{userID}:{context}`, ensuring that the same user talking to different agents gets independent conversation histories. See [Agent architecture](/docs/development/agent-architecture) for the session/runtime/memory design rules.

## Package Layout

```
cmd/stellad/             Entry point, server commands, service wiring
  store/               DBStore: the assembly layer over the domain packages
internal/
  platform/            Infrastructure that knows nothing about agents (see below)
    config/            Store interface, DBStore (PostgreSQL), Snapshot, types
    home/              POSIX workspace materialization, owner validation, deletion fencing
    blob/              Opaque bytes behind one interface (S3-compatible object storage)
    observability/     Process-global OpenTelemetry tracer and logger providers
    cli/               stellad command plumbing: dotenv and log level
    diagnostic/        Redacted rendering of sensitive values for operator output
    version/           Build version, set via ldflags
    xberg/             How Stella invokes the bundled Xberg CLI
  core/                Leaf kernels any internal package may import (see below)
    access/            Agent access decisions over an authz.Authority
    agentctx/          Agent/session context keys
    agenterr/          Shared sentinel errors
    providercred/      Per-agent provider credential resolution
  agent/               Service, ServiceManager, session registry, runtime, runner factory
    session/           Session lifecycle, ownership, kind/channel policy
    runtime/           Runner cache, turn execution, event persistence
    prompt/            System prompt builder and templates
    sandbox/           Core sandbox tools (bash, view_image)
    delegate/          Internal managed-session adapter and presets
    tracehook/         Agent trace hook: slog + OTel spans for LLM, tool, memory activity
  channel/             Channel interface, identity resolution, slash commands, notify
  memory/              Memory provider registry + implementations (lcm, simple)
  server/              HTTP API + embedded React SPA
  auth/                Login, sessions, and identity
  authz/               Shared authorization vocabulary (Authority, Action)
  controlplane/        Control-plane domain (providers, settings, plugins, channels)
  plugin/              Plugin machinery
    manifest/          Manifest-declared plugins, mise runtimes, overrides, reconciliation
    host/              Capability-scoped plugin platform host and durable plugin state
  model/               Which model runs, what it costs, what it embeds
    catalog/           models.dev snapshot, local overrides, and the effective-model merge
    usage/             Per-turn token and cost accounting
    embedding/         Embedding providers, indexing, storage
  skill/               Managed Skill authority, exact revisions, search, and loading
    access/            Who may see or change a skill
    policy/            Per-agent managed skill policy
  library/             Document library: raw storage, derivation, retrieval
    recally/           Read-later and feed backend over the same storage
  db/                  PostgreSQL (pgx/v5), goose migrations, sqlc queries, embedded runtime
  scheduler/           River-backed service (durable job scheduling for Web UI and native agent tools)
  tools/               Code generators run by mise tasks (toolgen, catalog/binary sync); not linked into stellad
pkg/
  ai/                  Message/Content types, Model, Provider interface, streaming events
  tools/               Tool interface and registry
  toolmeta/            Public generated-tool metadata and closed builtin inventory
  email/               Public Email DTOs and the HTTP/plugin call contract
plugins/
  channels/            Channel plugins (telegram, discord, qq, feishu, weixin)
  providers/           Typed provider definitions + LLM adapters (anthropic, openai, openai-response)
  sandbox/             Sandbox backend implementations
  email/               User-owned Email implementation, config, transport, and tools
```

Dependencies point one way, and `internal/boundary_test.go` holds the line in both directions. `pkg/` is the plugin-facing contract surface and never imports `internal/`. Channel plugins are adapters behind `pkg/channel` and `pkg/plugins` contracts, so all production code under `plugins/` may import only `pkg/**` and other plugin implementation packages. Production code under `internal/` may not import concrete non-channel plugins; existing composition paths may still depend on channel integrations. `cmd/stellad` is the composition root that imports both sides and wires implementations to contracts. `internal/platform/**` is the infrastructure floor: it may import only the standard library, third-party modules, `pkg/**`, and other `internal/platform/**`, so no platform package can reach up into a domain (`_test.go` files may additionally use the `internal/db/dbtest` harness). `internal/core/**` is the kernel: `platform`'s whitelist plus other `internal/core/**` and `internal/authz`. `internal/db` is deliberately not under `platform` — it implements `internal/auth`'s stores, so it depends on a domain. See [Go patterns](/docs/development/rules/go-patterns) for what belongs where.

## Configuration

Configuration is stored in PostgreSQL and accessed through the `config.Store` interface. There is no YAML config file; all settings (providers, agents, channels, scheduler) are managed via the admin API or database.

- **Store** (`config.Store`) -- Interface for reading and writing providers, agents, channels, users, and chat-agent bindings. Implemented by `DBStore`.
- **DBStore** (`config.DBStore`) -- PostgreSQL-backed implementation using sqlc-generated queries.
- **Snapshot** (`config.Snapshot`) -- Read-only view of configuration for a single agent. Assembled from the Store at pool creation time. Contains resolved provider credentials, model names, workspace path, system prompt, and runner settings. Passed to the runner factory and tools that need per-agent config.

## Home persistence and lifecycle

`internal/platform/home.WorkspaceManager` is the sole production materializer beneath one POSIX `STELLA_HOME`. PostgreSQL user, group, and Agent rows authorize deterministic local paths; the filesystem owns layout and bytes. A missing workspace for live owners is created, while a symlink, non-directory, unsafe ID, or replaced trusted root fails closed. Existing files are never registered into a PostgreSQL Home catalog because Phase 1 has no such catalog.

An explicit destructive user, group, or Agent delete fences local cached execution before deleting the owner in the existing database transaction. Physical bytes and inodes remain, but owner validation prevents later workspace access. A filesystem entry of any kind at `agents/{id}` reserves the global Agent ID. Assignment removal, member removal, Session archive, and Helm uninstall do not delete workspace bytes. This is a trusted-host, single-replica boundary; multi-replica, Kubernetes, and S3 storage authority require a future design.

## Composition & Lifecycle

`cmd/stellad` is the single manual composition root. There is no DI framework and no generic `Lifecycle` interface — subsystems are constructed and wired explicitly, in one place, so the wiring is auditable. Startup runs in strict phases, and each phase must complete before the next:

1. **Boot config** — `serverAction` parses `config.LoadServerConfig(os.LookupEnv)` and `oidc.LoadLoginConfig(os.LookupEnv, baseURL)` once, at the startup boundary. No other package reads the environment (a test tripwire enforces this, with a small allowlist for `STELLA_HOME`/OTel/runtime passthrough). The final base URL is resolved here and threaded down, so shared services are constructed with it directly — never a `localhost` placeholder mutated later.
2. **Build** — `setup()` constructs each subsystem once. The shared credentials/email/share/recally/MCP services are built a single time (each domain owns its own query set via a `*ForPool` constructor), so the same instance backs both the agent tools and the HTTP endpoints.
3. **Bind** — genuine back-edges are closed with one-shot, pre-start binds that reject a nil/duplicate/late bind: the PoolManager's `BindVaultEnvLoader`/`BindMCPToolProvider`/`BindOAuthRegistry` (before `StartAll`), the shared River client's `BindRiverClient` on the scheduler/goal/embedding/session-media services, and `AddBuiltinTool` (duplicate-checked, sealed by `StartAll`). Ordinary dependencies are constructor-injected, not bound.
4. **Validate / Seal** — `pluginhost.Seal()` validates every static registration and capability binding, then refuses further static registration; the dynamic desired-state surface (`ApplyChannel`/`RegisterManifestPlugins`) stays open. The admin server is built from an immutable, validated `server.Deps` via `server.New(ctx, deps)` which fails fast on a missing required dependency. `server.New` reads no environment, constructs no service, and has no setters.
5. **Observability** — global OTel tracing initializes before the serving phase, so no span-emitting component (agent runs via HTTP/channel ingress) starts before the exporter is installed.
6. **Run** — only now does the composition root start ingress, and only after every backend it depends on is up. First it wires the static callbacks (`notifier.SetAuthService`, the scheduler `OnJob` handler — both mutex-guarded one-time writes) and starts the one shared River client with scheduler, goal, and embedding workers, then starts the scheduler, goal dispatch tick, and embedding backfill; the scheduler handler is wired **before** River starts, since River may run a persisted job the instant it starts. Only then does ingress come up — the group-dispatch loop, the managed channel runtimes, and finally `httpSrv.Serve` (the listener is bound earlier but not served). The root owns one `errgroup`: `httpSrv.Serve` and `groupDispatcher.Run(ingressCtx)` run under it. Expected shutdown errors normalize to `nil` (`http.ErrServerClosed`, `context.Canceled`); any other component error cancels its peers and becomes the root error. Component constructors start no goroutines — background loops are entered by an explicit blocking `Run(ctx)` or a `Start` owned by the root (e.g. the trace hook's idle-session reaper).

**Immutable Server Deps.** `server.Deps` is a value struct of application services: Account, Profile, Project, Inbox, Agent/Session/Skill access, Group, control-plane, and shared capability services. `internal/server` has no persistence store, query handle, or pool; `DBPinger` is its only database-shaped dependency and is limited to liveness probes. Terminal AST guards reject broad `Deps` fields, server persistence selectors, and `sqlc`/`pgxpool` imports; their counterexamples cover nested fields, aliases, handler query use without an import, DTO-only imports, and dot imports. Optional capabilities are nil-tolerant and degrade through one centralized 503 mapping.

**Authorization.** Agent HTTP, webhook, and channel entry points use the authoritative `internal/core/access` domain service. Session and workspace use cases use `internal/agent/session/access`: it loads durable owner, agent, kind, and lifecycle facts before creating a scoped registry access, then decides Agent, Session, and Workspace against its own static rules over the immutable `authz.Authority`. The former RBAC/ABAC policy engine and the temporary generic policy engine are both gone; there is no separate central decision path. Authorities are minted only by trusted identity adapters (`internal/auth`, `internal/credential`, `internal/authz`) and the durable worker/group adapter in `internal/core/access`; request body/path fields can never mint or overwrite an actor.

The execution domains follow the same shape: Account, Profile, Project, Inbox, Group, Workflow, Scheduler, Goal, and Skills each expose an application service that owns its use cases and returns domain values to transports, never generated API types. Every HTTP, channel, tool, and worker use case binds one immutable Authority through that service before loading or mutating a protected resource; transports do not preload resources for optional authentication. A cross-resource agent gate is folded in by calling `agentaccess` directly with the same Authority. Durable workers reconstruct the owner/executor Authority from persisted trusted state and re-decide on every action. `admin` is a superuser each domain honors via `Authority.IsAdmin()` rather than scattered `role == admin` checks.

The user-capability domains are all user-owned — a delegated agent turn acts with its user's access rather than an executor confinement (an agent shares a user's secrets, mail, connections, and reading library) — but they split by shape. **Vault owns scope rules**: `vault.Service` binds the Authority per use case and decides its own static rules, because vault entries have real `user`/`user_agent`/`system`/`system_agent` scope distinctions (`user`/`user_agent` are user-owned with an agent-read gate folded in; admin-managed `system`/`system_agent` are reachable only by an admin). It preserves at-rest encryption, no secret read-back, reserved-name guards, and runner invalidation. **Connections, Email, Share, and Recally are coarse capabilities**: each is a per-user capability with no scope or admin distinctions, so `connections.Service`, `share.Service`, and `recally.Service` bind one trusted `authz.Authority` and enforce ownership through user-scoped durable queries. `plugins/email` owns the implementation and captures the user resolved from a trusted context adapter in `internal/plugin/host`; `pkg/email` carries only the shared DTOs and `Service.Access(context.Context)`, while HTTP installs its verified Authority and tools preserve `ToolIdentity`/`ToAuthority` and the existing error mapper. Email receives only a composition-root reader for that user's `EMAIL_CONFIG`, while Vault remains the storage and host-side authorization boundary. OAuth bundles and flows are keyed by user, shares are deleted `WHERE user_id = ?`, and recally rows are uid-scoped so a foreign row is simply not found. Operations keyed only by a parent id (recally article content, feed entries) prove parent ownership with a uid-scoped load first, and Share artifacts keep os.Root workspace confinement for an agent-scoped actor. Several surfaces stay deliberately trusted or public: vault's host-side callers (MCP, OAuth, email config, channel config, sandbox env, key provisioning) use the raw service methods; the OAuth callback and token-refresh paths are keyed by the flow/user, not a live request; and the public share view is an unguessable capability URL authorized by token hash plus expiry with no session. See the [authorization guide](/docs/development/authorization) for the resource matrix and recipes.

The control-plane domains close the loop. Provider, Settings, Plugin, and Channel management run through `internal/controlplane` — a `Begin(authority) → Access` domain that replaces the old `requireAdmin` gate plus direct `config.Store` access. They are admin-only: `Begin` validates the Authority and requires `IsAdmin()` once, so an Access exists only for an admin. Channel management is one capability: its single decision includes persisting a channel and enabling/applying its channel plugin, not a second Plugin decision. The plugin host is capability-scoped to match: `pluginhost`'s `Platform` is no longer ambient — only static Go `PluginInfo.RequiredCapabilities` declarations can reach host ports, and each is validated against an injected backing service before a managed runtime starts. Manifest plugins can describe plugin traits but cannot request host ports. With this, both the legacy `auth.PolicyEngine` and the temporary generic policy engine are gone: authorization is `internal/authz` (the shared `Authority`/`Action` vocabulary) plus each domain's own static rules.

**Static vs dynamic.** Boot-static capabilities are bound once before start and then sealed. Live reconfiguration (plugin tool/hook/provider reloads, agent sync, runner invalidation) is a distinct surface that stays available after start and applies atomically — it never re-runs the one-shot binds.

**Shutdown ordering.** The first `SIGINT`/`SIGTERM` starts a graceful drain (a second collapses to a hard stop). The `drainSequence` runs: mark `/readyz` unready and signal SSE streams → **stop every non-HTTP ingress source** (group-dispatch acceptance, channel bot pollers, and the scheduler/goal/embedding/session-media River periodics + one-time dispatch), each via an idempotent stop-once closure, so no new work or periodic fires after the drain begins → drain in-flight HTTP within `STELLA_HTTP_SHUTDOWN_TIMEOUT` (force-close on deadline) → wait for accepted agent turns that hold no HTTP connection (channel messages, webhook runs, scheduler run-now) within the same budget → cancel the work context, after which River drains its in-flight jobs within the soft-stop budget and the LIFO defer chain reverse-closes the subsystems. The main goroutine joins the drain supervisor before its teardown defers run, so process exit never races the drain; a second signal collapses the shared budget and hard-stops. The group-dispatch loop runs on a dedicated `ingressCtx` (a child of the errgroup context) so it can be halted without cancelling the work context; outbound dependencies (pools, notifier) stay alive until that final cancel, so work accepted before the drain can still complete and deliver. The same stop-once closures back both `stopIngress` and the reverse-defer cleanup, so the crash / startup-error path tears down safely with no double-stop. A subsystem crash cancels the errgroup and tears down without a readiness drain.

## Multi-User Multi-Agent Routing

Each incoming message goes through a two-step resolution before reaching the agent loop:

1. **User resolution** (`channel.ResolveUser`) -- Upserts the sender by external platform ID, returning a `config.User` record with a stable internal user ID.
2. **Agent resolution** (`channel.ResolveAgent`) -- Determines which agent handles this message:
   - In DMs, the user's `default_agent_id` is used.
   - In group chats, a `chat_agents` binding maps `(platform, chat_id)` to an agent.
   - If neither is set, the first enabled agent is used as fallback.

The resolved user and agent are bundled into a `ResolvedChat` struct that threads through all handler and command paths. This struct holds the target `Service`, the `User`, the `AgentID`, and the `SessionKey`.

The `ServiceManager` (implemented by `PoolManager`) maintains a `map[agentID]*Service` and lazily creates services on first access. Each service is configured with its agent's `Snapshot` (model, credentials, workspace, system prompt) via the runner factory.

### Agent Routing

Channel configuration selects a dedicated agent when one is bound. Otherwise, direct messages use the user's default agent, falling back to the first enabled agent. Each group message wakes every eligible member agent, and each member's local deterministic triage decides whether it speaks.

## Providers

LLM providers use a typed, compiled-in registry. Three built-in providers ship with Stella:

| Provider          | API                  | Use Case                                                   |
| ----------------- | -------------------- | ---------------------------------------------------------- |
| `anthropic`       | Messages API         | Claude models                                              |
| `openai`          | Chat Completions API | GPT models                                                 |
| `openai-response` | Responses API        | OpenAI-compatible services (Perplexity, Together.ai, etc.) |

Each provider implements the `ai.ProviderAdapter` interface for streaming responses and optionally `ai.ModelLister` for model discovery. Provider adapters can encode `ImageContent` as their native image format (base64 blocks for Anthropic, data URI image_url for OpenAI), but the agent boundary creates it only for a model that declares image input and only during that image's active turn. Historical images arrive at adapters as text baselines.

Providers live in `plugins/providers/` and export a `providers.Definition`. The composition root in `cmd/stellad` lists those definitions explicitly, validates the registry, and injects it into the runner and control plane. Adding a provider therefore requires both its package and deliberate composition-root wiring. Provider packages do not import `internal/**`. See [plugin-system](/docs/development/plugin-system) for details.

Managing providers (like settings, plugins, and channels) is a control-plane operation authorized through `internal/controlplane`, not a bare role check. It is admin-only: `Begin` requires `IsAdmin()` before minting an Access.

### Agent Provider credential overrides

Provider metadata and the default key remain global control-plane state. A
separate `agent_provider_credential` relation stores an encrypted override for
`(agent_id, canonical provider_id)`. `providercred.Service` is the only plaintext
encryption boundary and uses the Vault system cipher; neither `config.Agent` nor
ordinary Agent projections contain credential fields.

The credential-aware Snapshot loader decorates the global Snapshot at the Agent
boundary. It replaces only the API key on every canonical and type-alias entry
for the referenced Provider, including the legacy default credential field.
Provider type, base URL, model catalog, and enabled state never move into Agent
scope. Missing or deleted rows use the global key; a referenced row that cannot
be decrypted fails closed instead of silently falling back. All host-side Agent
consumers share this loader, including Runner, memory summarization, intent and
semantic routing, Reflect, and Vision.

Safe metadata follows Agent Read access. Mutation requires Agent Manage, which
allows administrators and the persisted Agent creator but not assigned
non-creators. A mutation commits first and then calls targeted `SyncAgent`; it
does not reload global Providers. The write-only HTTP subresource exposes
paginated List, Get, PATCH rotation, and idempotent DELETE fallback.

## Tools

The Runner injects tools into LLM calls. Tools follow a common interface defined in `pkg/tools/`. The `tools.Definition` type is a type alias for `ai.ToolDefinition`, keeping domain packages decoupled:

```go
type Tool interface {
    Definition() tools.Definition
    Execute(ctx context.Context, args map[string]any) (string, error)
}
```

### Core sandbox tools

| Tool         | Availability | Description                                                              |
| ------------ | ------------ | ------------------------------------------------------------------------ |
| `bash`       | Always       | Execute shell commands, including textual file reading and editing       |
| `view_image` | Always       | Route image inspection to pixels, untrusted text, or an actionable error |

The public web is not a tool: the `web` skill's scripts (search, fetch, site scripts) run inside the sandbox through `bash`; see [Web research](/docs/guides/web-research).

The core local-workspace tools run through a Docker sandbox backend. `bash` executes via `Session.Exec` and is the general file-operation tool; its description carries the contract that dedicated read/write/edit schemas used to encode. `view_image` uses the mediated `Session.Files` capability with process-visible paths and routes based on the effective parent model: supported parents receive verified pixels, while other parents receive untrusted text from the vision service or generic baseline, or an actionable error. Provider backing paths never enter the tool layer. Runner startup fails closed when Docker is unavailable.

### Sandbox

The sandbox system provides process, filesystem, and network isolation for agent tool execution. All core tools share the same `sandbox.Session` per runner: `bash` uses `Session.Exec`; `view_image` uses `Session.Files`. Public policy contains only process-visible roots; each backend owns the physical mount mapping and rooted file capabilities. Concrete backends live in `plugins/sandbox/`, export public sandbox interfaces, and are adapted into a validated registry by `cmd/stellad`; `internal/agent/sandbox` selects only from that injected registry. Runner startup fails closed when the selected backend is unavailable. See [Sandbox Backend Abstraction](/docs/development/sandbox) for the full Session interface, execution mediation, fail-closed behavior, and exception boundaries.

Sandbox tools (`bash`, `view_image`) live in `internal/agent/sandbox/`; public-web research is a skill, not a tool package: `plugins/tools/bun/skills/web/` ships the `web` skill (`web.ts` search/fetch plus site scripts) and `cmd/stellad` registers the builtin tools in the catalog. Declarative CLI integrations use the built-in manifest. See [plugin-system](/docs/development/plugin-system) for the extension boundaries.

### Session Tool

The model-facing `session_*` tools own Session management, bounded inspection, creation, and synchronous communication. Content recall belongs to the `memory` tool:

- `session_list` lists recent, active, or archived Session cards without semantic search.
- `session_get` returns metadata and context statistics, and pages bounded logical turns.
- `session_create` opens a focused persistent Session and can apply an internal preset.
- `session_send` runs one turn on an owned sendable Session, including legacy delegate Sessions.

The runtime keeps `DelegateTool` and delegate Session kinds as internal compatibility machinery for presets and existing IDs. It does not register `delegate` in the model tool registry.

Agent sends first persist an input row, then enter a process-local per-Session FIFO before the standard runtime admission guard. The queue bounds pending depth and admission hold time, propagates the source context, and never replaces the runtime correctness guard. LCM claims the row in the same transaction that appends the transcript message. Startup recovery reauthorizes and append-only delivers pending rows; it never starts a model or tool turn. Nested calls carry depth and ancestry in context, reject cycles, inherit the root deadline, and share a 16-call root-turn budget across sibling and nested calls. Agent-originated input persists its actor and source Session; prompt rendering marks it as information rather than human authority. Synchronous Session turns never publish to an external channel implicitly, and inbox durability does not make replies or execution durable.

### Builtin Shared Tools

| Tool               | Condition                         | Description                                                                         |
| ------------------ | --------------------------------- | ----------------------------------------------------------------------------------- |
| `memory`           | Always                            | Unified search and read across conversation and durable memory                      |
| `session_*`        | One-to-one agent sessions         | Session listing, bounded inspection, creation, and synchronous sends                |
| `skill_*`          | Always                            | Search installed Skills and load one exact selected revision                        |
| `scheduler__job_*` | Scheduler plugin enabled          | Schedule tasks: one tool per action (`scheduler__job_create`, `_list`, `_pause`, …) |
| `notify`           | Gateway mode + channel configured | Send notifications via dispatcher                                                   |

Memory is two tools over one shared `memory.Recall`: `memory_search` federates snapshot-visible LCM messages/summaries with durable facts, profile, soul, and constraints; `memory_read` resolves an opaque result ref or a well-known identity/constraint/history ref. Dynamic reads reauthorize through Session access, and summary reads preserve LCM describe/expand through bounded child refs. Transcript statistics, whole-message reads, and durable profile, soul, or constraint management are not tools at all — they belong to the internal, Reflect, or manual surfaces that own their authorization.

## Session Lifecycle

1. Channel resolves user and agent, producing a `ResolvedChat`
2. `ResolvedChat.Chat(ctx, message)` is called -- message is `string` (text) or `[]ContentBlock` (multimodal)
3. `Service.Chat` resolves or creates a session through `session.Registry` using the scoped key
4. `runtime.Runtime` acquires or creates a runner for the session, configured with the agent's Snapshot
5. Runner streams events back through a channel
6. On idle timeout, runners are reaped; sessions persist to PostgreSQL via `memory.Provider`

See [session-compaction](/docs/development/session-compaction) for history management.

## Channel Interface

All messaging platforms implement the `channel.Channel` interface:

```go
type Channel interface {
    Name() string
    Start(ctx context.Context) error
    Stop()
    Notify(ctx context.Context, n Notification) error
}
```

Shared command logic for `/new`, `/compact`, and `/abort` lives in the channel coordination layer, which each channel delegates to for the core logic; adapters can provide platform-specific help and identity commands. `/new` rotates the chat onto a fresh session — the previous one is archived, never deleted — and runs as a control operation on the same per-session queue as chat turns, so it never races an in-flight turn. `/new` applies to direct messages only: a group's context is shared, so a group `/new` is refused before the shared event log is written, which keeps the refused command out of every agent's context. Chat turns are serialized per resolved Stella session so overlapping channel messages cannot race the same session history; `/abort` cancels the currently running turn for that session.

### Channel ingress ownership

Stella supports one server replica ([#637](https://github.com/CherryHQ/stella/issues/637)). The Helm chart enforces `replicaCount: 1` and a `Recreate` rollout, so managed channel bot pollers start unconditionally once their dependencies are wired. Running two `stellad` processes against the same channel configuration is unsupported: Telegram may return 409 and Discord, QQ, Feishu, or WeChat may duplicate delivery. Multi-replica channel ingress needs a complete offset and fencing design; a database lease alone is not that design.

During graceful drain, `pluginHost.Quiesce` stops new channel polling while accepted work and notifier senders remain alive. Final `pluginHost.Stop` runs only after River drains, preserving outbound delivery for accepted work.

Delivery guarantees, stated honestly:

- **Group messages are at-least-once.** Ingest is durable (event-log dedup) and dispatch is a durable outbox (`ctx_group_outbox`) claimed under a CAS lease, so a lost dispatcher hands the work to another. A duplicate **platform** send is possible when a dispatch lease expires and the message is re-published after a partial send.
- **Inline DM turns are at-most-once.** They have no durable queue: a turn interrupted by a crash is dropped, not retried.
- **No exactly-once is claimed or implemented** anywhere in channel ingress.

## Admin API

The `internal/server/` package provides an HTTP API and embedded SPA for managing the system. It translates HTTP to application-service calls and API DTOs; persistence and query handles stay outside the package. Control-plane management — LLM providers, deployment settings, plugins, and channels — is authorized through `internal/controlplane` (admin-only: `Begin` requires `IsAdmin()`), not a bare role check.

## Notification Flow

```
Agent notify tool      --> Dispatcher --> Channel (Telegram/Discord/QQ/Feishu/WeChat)
Scheduler job result   --> Dispatcher --> Channel (Telegram/Discord/QQ/Feishu/WeChat)
```

The dispatcher is created early in setup, but backends are registered later when gateway services start. The ServiceManager wires per-agent notification tool injection through the `BuiltinToolsFactory`, keeping notifications in the always-on builtin tool set while external tools remain plugin-managed.
