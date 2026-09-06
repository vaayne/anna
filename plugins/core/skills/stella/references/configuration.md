# Configuration reference

All configuration is stored in PostgreSQL. Stella can run an embedded PostgreSQL cluster whose data directory lives under `$STELLA_HOME`; run `stellad postgres download` first if the runtime is not installed. Set `STELLA_DATABASE_URL` to point at an external PostgreSQL server instead.

The easiest way to configure stella is to run `stellad server` and open `http://localhost:25678`. Use `--port` to change the port.

## Quick start

1. Run `stellad server` and open `http://localhost:25678`
2. Add a provider (e.g., "anthropic" with your API key)
3. Create or edit an agent (set provider, model, system prompt)
4. Configure channels (Telegram token, etc.)
5. Restart: `stellad server`

On first run, Stella creates an enabled `stella` agent without a provider or model. Add a provider and choose its model for Stella in the Web UI before chatting.

## Conversational Settings

An Agent manager can opt one Agent into a limited subset of conversational
Settings tools in **Profile → Configuration → Advanced configuration**. Built-in
Stella starts enabled, including after an upgrade; every other new Agent starts
disabled until its manager opts in. When enabled, these cold Code Mode tools are discovered only in a signed-in,
foreground one-to-one `main` or `chat` session. They remain unavailable in group
or guest chat, webhooks, scheduler/task/delegate workers, and Agent-originated
`session_send`. Catalog visibility is not permission: every call rechecks the
durable Agent setting, direct human Authority, and the domain's normal access
policy.

### Capability matrix

| Area                     | Exact tools                                                                                                                                                                  | Who and what they can manage                                                                                                                                                                                                                                                                                                   |
| ------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Agents                   | `settings_agent_list`, `settings_agent_get`, `settings_agent_create`, `settings_agent_update`, `settings_agent_delete`                                                       | The caller's normally readable or manageable Agents. Existing Agent policy decides whether a caller may create, manage, or delete a given Agent. Agent workspace, sandbox policy, assignments, and provider credentials are excluded.                                                                                          |
| Per-Agent tool overrides | `settings_agent_tool_list`, `settings_agent_tool_update`, `settings_agent_tool_delete`                                                                                       | An Agent the caller may manage. Update sets one override; delete removes it and restores the normal visibility decision.                                                                                                                                                                                                       |
| Library files            | `settings_library_file_list`, `settings_library_file_get`, `settings_library_file_upload`, `settings_library_file_delete`                                                    | `user` and `user_agent` scopes allowed to the caller. An administrator may also use `system` and `system_agent`; an Agent target is separately authorized.                                                                                                                                                                     |
| Managed Skills           | `settings_skill_list`, `settings_skill_get`, `settings_skill_create`, `settings_skill_update`, `settings_skill_delete`                                                       | The same `user`/`user_agent` scopes, plus `system`/`system_agent` for an administrator and an authorized Agent target. These are managed-Skill records, not `skill_installed_search` or `skill_load`.                                                                                                                          |
| Providers                | `settings_provider_list`, `settings_provider_get`, `settings_provider_create`, `settings_provider_update`, `settings_provider_delete`                                        | Administrator only. The view is redacted and exposes `credential_configured`, never an API key or credential reference.                                                                                                                                                                                                        |
| Default models           | `settings_default_model_get`, `settings_default_model_update`                                                                                                                | Administrator only. Covers the deployment's default, thinking, strong, fast, vision, and embedding model roles.                                                                                                                                                                                                                |
| Embedding settings       | `settings_embedding_setting_get`, `settings_embedding_setting_update`                                                                                                        | Administrator only. Covers enabled state, dimension, and normalization, not a separate provider credential or endpoint.                                                                                                                                                                                                        |
| Plugins                  | `settings_plugin_list`, `settings_plugin_enable`, `settings_plugin_disable`                                                                                                  | Administrator only. A plugin is addressed by `kind` and `name`; there is no `plugin_get` or arbitrary plugin-config write tool.                                                                                                                                                                                                |
| MCP registrations        | `settings_mcp_server_list`, `settings_mcp_server_get`, `settings_mcp_server_create`, `settings_mcp_server_update`, `settings_mcp_server_delete`, `settings_mcp_server_probe` | `user` and `user_agent` scopes allowed to the caller; an administrator may also use `system` and `system_agent`, with a separately authorized Agent target. Probe connects, lists the server's tools, and persists status plus the tool catalog; a failed probe still returns the registration with a redacted `status_error`. |

### Read, version, then mutate

A mutation that replaces or deletes an existing resource requires the opaque
`version` returned by the corresponding read:

- `settings_agent_update` and `settings_agent_delete` use `settings_agent_get`.
- `settings_agent_tool_update` and `settings_agent_tool_delete` use `settings_agent_tool_list`. For a
  first override, use the listed opaque absent version; after that use the
  override's returned version.
- `settings_library_file_delete`, `settings_skill_update`, and `settings_skill_delete` use their `get`
  result.
- `settings_provider_update` and `settings_provider_delete` use `settings_provider_get`.
- `settings_default_model_update` and `settings_embedding_setting_update` use their `get`
  result.
- `settings_mcp_server_update` and `settings_mcp_server_delete` use `settings_mcp_server_get`.
- `settings_mcp_server_probe` addresses a registration by `id` and needs no version; it refreshes the probe `status` and the persisted tool catalog.

A version conflict means the resource changed. Read it again before deciding
whether to retry. Creation and Library upload do not take an expected version;
their results include the server-selected ID and current version. Plugin enable
and disable address `kind` plus `name` and do not take an expected version.

A managed Skill update cannot change its scope or owner. To promote a Skill,
keep the source in place while creating it in the target scope from the same
directory, verify the target with `settings_skill_get`, then delete the source
using its version. The same name may exist in different scopes. If create says
the name already exists in the target scope, list that scope and update the
matching Skill instead of retrying create.

### Secrets and trust boundaries

No conversational Settings tool accepts an API key, bearer token, credential
reference, or another secret. Provider and Agent Provider credentials, MCP
bearer credentials, and every credential set/replace/delete action remain
Web UI/API-only. A Provider created through a tool has no key. A Provider whose
key is already configured cannot change endpoint origin through a tool; use the
Web UI to intentionally change its credential binding.

`settings_mcp_server_create` always creates a no-auth registration. `settings_mcp_server_update`
does not accept auth type or token. A bearer-backed registration may change
limited safe metadata, but cannot move scope or owner or change endpoint origin
through a tool. MCP list/get expose redacted metadata such as `auth_type`, `status`, and
`credential_configured`, never the bearer or credential reference.

The following Settings areas remain Web UI/API-only: Account, Users,
Provisioning, Channels, Webhooks, OAuth connection configuration, arbitrary
plugin configuration, Agent workspace and sandbox settings, and every
credential-binding change. Existing `oauth_*` and `vault_secret_*` tools are
separate capabilities, not a path to bind credentials to Providers, Agents, or
MCP registrations.

### Result and source bounds

- Agent, Provider, Plugin, and MCP lists return at most 50 entries; a
  `truncated` flag reports that more entries exist. Agent and MCP calls accept
  a maximum `limit` of 50.
- Library list uses `page_size` from 1 through 100 and returns
  `next_page_token` when another page exists. Library get/list/upload results
  are metadata only, never raw document bytes.
- Managed Skill get/list/create/update results expose safe metadata and file
  names, never file contents. Library upload reads a sandbox file up to 25 MiB;
  managed Skill create/update reads a complete Agent Skills directory or ZIP
  archive from `content_path`, including `SKILL.md` and optional resources. A package may
  contain at most 512 files, 32 MiB per file, and 32 MiB total. Create derives
  its name, description, and invocation setting from `SKILL.md`; update replaces
  the complete stored package and requires the same Skill name.
- Every Code Mode invocation, child result, and final result has a 1 MiB
  payload ceiling. Treat a bounded or truncated response as incomplete rather
  than assuming it is a complete export.

## Code Mode

Code Mode is the only tool path; there is no setting to enable it. Its production surface is Hot: `bash`, `memory_search`, `memory_read`, `skill_load`, and `view_image` when available stay directly callable, while `code` reaches the complete authorized catalog and keeps cold schemas outside provider context. Direct and child calls share authorization, hooks, auditing, redaction, sandbox, and tool execution.

The routing rule is deliberately narrow:

1. Directly exposed tools handle standalone work. Hot keeps `bash`, `memory_read`, `memory_search`, `skill_load`, and `view_image` directly callable. Use direct `bash` for standalone or potentially long-running shell, file, git, package, script, and process work; never wrap a standalone direct call in Code.
2. Code handles cold tools and short chains. Use it for a tool that is not exposed directly, or when intermediate results should stay between tools. Shell work inside Code uses `tools.invoke("bash", ...)`; the complete chain must fit Code's 30-second wall-clock budget.
3. Discovery only supplies missing information. Names exposed directly or documented by a loaded skill are exact. Search when the capability or name is unknown; describe directly when the exact name is known but its input schema is not. `tools.search(query, offset?)` returns up to 20 summaries, and a non-empty search with at most three total matches includes `inputSchema`. Describe a selected search result only when it omitted its schema. An empty query lists the catalog and pages expose `hasMore` / `nextOffset`; do not enumerate it as routine discovery.

`STELLA_EVAL_CODE_TOOL_SURFACE=hot|bash|only` is an internal evaluation hook for same-binary behavior comparisons. It is unsupported as an operator rollout setting; production Code Mode uses Hot, and invalid values stop startup.

`tools.invoke(name, args?)` returns a structured ToolValue. Use `tools.text(value)` for text blocks and `tools.json(value)` for JSON text, including caught `ToolInvocationError.value`. The VM has no ambient filesystem, process, network, timer, or module-import capability; `tools.invoke("bash", ...)` uses the same sandbox as direct `bash`. Keep large content in sandbox files and use documented path inputs such as Recally `content_path`, rather than copying bytes through JavaScript. Code has fixed execution and payload limits and is process-internal capability isolation, never a general user-code sandbox.

## Database tables

Core persisted configuration lives in PostgreSQL tables with domain-specific ownership:

| Table              | Purpose                                                                                     |
| ------------------ | ------------------------------------------------------------------------------------------- |
| `app_setting`      | Deployment-wide key-value settings                                                          |
| `agent`            | Agent definitions, model selection, system prompt, and workspace policy                     |
| `provider`         | Provider type, base URL, models, enabled state, and global credentials                      |
| `plugin`           | Managed plugin configuration and enabled state; providers and sandbox backends are not rows |
| `auth_user`        | User accounts and default-Agent preference                                                  |
| `channel`          | Channel instances, configuration, enabled state, and optional dedicated Agent               |
| `channel_agent`    | Per-channel chat or group Agent assignment                                                  |
| `ctx_agent_memory` | Per-user-per-Agent identity, constraints, profile, and memory snapshot state                |

## Multi-agent setup

Each agent has:

- A global Provider + model selection, with an optional API-only key override
- A system prompt (personality/identity)
- A user-independent definition and administrator-managed skills area
- A separate sandbox workspace for each user or channel group

Inside a sandbox, `$HOME` is that principal's per-agent workspace, not the
operator's `$STELLA_HOME/agents/{agent_id}` directory.

Create agents via the Web UI or directly in the database.

Provider type, base URL, models, enabled state, and default key are global
administrator configuration. Enterprise provisioning may set a write-only key
override through `POST /api/agents` or the Agent Provider credential
subresource. Override precedence is Agent key, then global Provider key;
deleting the override restores fallback. Safe metadata follows Agent Read, while
only administrators and the persisted Agent creator may mutate it. The same key
resolution applies to every host-side Agent model call, including Vision when it
uses that Provider. Do not place overrides in sandbox environment variables.

## Channel configuration

Channels are stored in the `channel` table. Each row is a channel instance with an `id`, platform `type`, optional dedicated `agent_id`, enabled flag, and JSON config. Stella does not create channel instances on startup; configure them via the Web UI.

**Telegram config fields:**

- `token` -- Bot token
- `channel_id` -- Broadcast channel ID or @username
- `allow_group` -- Accept messages from groups the bot was added to; defaults to `false`, which rejects all group messages
- `allow_dm` -- Accept private messages and account linking; defaults to `true`
- `allow_unlinked_dm` -- Allow persistent restricted guest private messages; defaults to `false`
- `guest_message_limit_per_minute`, `guest_max_per_channel`, `guest_retention_days` -- Guest resource limits
- `require_mention` -- Require a bot mention in group chats; defaults to `true`
- `enable_notify` -- Allow notify output for this channel

Channel access is enforced by Stella's trusted Authority-based domain services; notification targets are resolved from linked identities.

**Discord config fields:**

- `token` -- Bot token
- `allow_group` -- Master switch for server channels; defaults to `false`, which disables all guild messages but not direct messages
- `allow_all_guilds` -- Dangerous: skip the allowlist below and accept every server this bot joined; defaults to `false`. With `allow_group` on, `allow_all_guilds` off, and every allowlist empty, no guild message is served (fail closed)
- `allowed_guild_ids` -- Guild (server) IDs allowed to use the bot; defaults to empty
- `allowed_channel_ids` -- Channel IDs allowed to use the bot, matched against a thread's own ID or its parent channel ID; defaults to empty
- `allowed_user_ids` -- Discord user IDs allowed to use the bot in server channels; defaults to empty
- `allowed_role_ids` -- Discord role IDs allowed to use the bot in server channels, matched against the message author's guild roles; defaults to empty
- `allow_dm` -- Accept account linking and linked-user direct messages; defaults to `true`
- `allow_unlinked_dm` -- Allow persistent restricted guest direct messages on the channel-bound agent; defaults to `false` and requires `allow_dm`
- `guest_message_limit_per_minute` -- Per-guest message and command limit; defaults to `10`
- `guest_max_per_channel` -- Durable guest identity cap for one channel; defaults to `1000`
- `guest_retention_days` -- Inactivity period before the daily purge deletes a guest and its sessions; defaults to `30`
- `require_mention` -- Only process guild messages that mention the bot; defaults to `true`

Guest direct messages retain and compact conversation history but have no profile, reflection, tools, skills, files, workspace, plugins, or delegation. Guests can only use `/link`, `/help`, `/new`, `/compact`, and `/abort`; linking does not merge old guest history. Guest rate, count, and retention limits reduce abuse but enabling the feature still exposes model usage publicly, so warn about cost and security and use a dedicated guest-safe agent whose base prompt contains no secrets.

**QQ config fields:** `app_id`, `app_secret`, `enable_notify`

**Feishu config fields:** `app_id`, `app_secret`, `encrypt_key`, `verification_token`, `enable_notify`, `tenant_key`, `auto_provision`, `allow_group`, `allow_dm`, `allow_unlinked_dm`, `guest_message_limit_per_minute`, `guest_max_per_channel`, `guest_retention_days`, `require_mention`

Feishu `allow_group` is one fail-closed switch for every group the bot was added to and defaults to `false`. Direct messages default on, group mentions default required, and restricted guest direct messages default off. Guest sessions use the same isolation and resource limits described for Discord.

**DingTalk config fields:** `client_id`, `client_secret`, `allow_group`, `allow_dm`, `allow_unlinked_dm`, `guest_message_limit_per_minute`, `guest_max_per_channel`, `guest_retention_days`, `require_mention`

DingTalk uses Stream mode and requires no public callback URL. `allow_group` is one fail-closed switch for group conversations and defaults to `false`. Text messages, direct messages, group @mentions, account linking, and restricted guest DMs are supported. Notifications require a temporary session Webhook learned from a recent inbound message and stop working after restart or expiry until the user or group messages the bot again.

## Login providers

Stella supports local password login, one external OIDC provider, and multiple OAuth login providers.

Local password login is enabled when `OIDC_ISSUER_URL` is not set. The first local registrant bootstraps the admin account; after that, local self-registration is closed unless `LOCAL_PASSWORD_ALLOW_REGISTRATION=true` is set. `LOCAL_PASSWORD_ALLOWED_EMAIL_DOMAINS` optionally restricts self-registration by submitted email domain; it does not verify mailbox ownership and does not affect existing-user login. The old `LOCAL_OIDC_*` names are compatibility fallbacks only. `STELLA_TRUSTED_PROXIES` is a comma-separated list of proxy IPs/CIDRs whose `X-Forwarded-For`/`X-Real-IP` headers may be used for authentication rate limiting.

Standard external OIDC login uses `OIDC_*` env vars (`OIDC_PROVIDER_NAME`, `OIDC_ISSUER_URL`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`, `OIDC_REDIRECT_URL`, `OIDC_SCOPES`). Setting `OIDC_ISSUER_URL` replaces local password login on the login page.

OAuth login supports multiple providers through env vars:

```bash
AUTH_OAUTH_PROVIDERS=google,github,feishu
AUTH_OAUTH_FEISHU_CLIENT_ID=cli_xxx
AUTH_OAUTH_FEISHU_CLIENT_SECRET=...
AUTH_OAUTH_FEISHU_ALLOWED_TENANT_KEYS=tenant_key
```

Built-in OAuth provider IDs: `google`, `github`, `feishu`. Google uses OIDC discovery and verified ID-token email, and must be restricted with `AUTH_OAUTH_GOOGLE_ALLOWED_EMAIL_DOMAINS`; tenant keys are not supported for Google login. Every OAuth provider must set either `AUTH_OAUTH_{PROVIDER}_ALLOWED_EMAIL_DOMAINS` or a provider-supported tenant allowlist; Feishu requires tenant keys because Feishu email fields are directory data, not live mailbox verification. Generic OAuth providers require `email_verified: true` by default; set `AUTH_OAUTH_{PROVIDER}_REQUIRE_EMAIL_VERIFIED=false` only for trusted providers that do not expose that claim. If Feishu does not return an email, Stella uses a stable internal email like `union_id@tenant_key.feishu.local`; configuring `AUTH_OAUTH_FEISHU_ALLOWED_EMAIL_DOMAINS` makes a real matching Feishu email required.

## Settings (key-value)

Global settings are stored in the `settings` table as JSON values:

| Key         | Purpose                                           |
| ----------- | ------------------------------------------------- |
| `runner`    | Idle timeout, delegate timeout, compaction config |
| `scheduler` | Scheduler enabled flag, data directory            |
| `plugins`   | Array of plugin configs (path + optional config)  |

## Directory layout

All paths are relative to `$STELLA_HOME` (`~/.stella` by default).

| Operator path                                      | Purpose                                                                                                         |
| -------------------------------------------------- | --------------------------------------------------------------------------------------------------------------- |
| `postgres/`                                        | Embedded PostgreSQL data directory (all config; absent when `STELLA_DATABASE_URL` points at an external server) |
| `pg-runtime/`                                      | Downloaded embedded PostgreSQL runtime; recreate with `stellad postgres download`                               |
| `cache/sandbox-tmp/`                               | Docker sandbox temporary directories; scratch, removed when stale                                               |
| `.agents/db-skills/`                               | Mutable system Skill authority: immutable digest revisions plus atomic current selectors                        |
| `agents/{agent_id}/`                               | User-independent Agent definition tree                                                                          |
| `agents/{agent_id}/.agents/skills/`                | Mutable system-Agent Skill authority: immutable digest revisions plus atomic current selectors                  |
| `users/{user_id}/.agents/skills/`                  | Mutable user Skill authority: immutable digest revisions plus atomic current selectors                          |
| `users/{user_id}/.agents/agent-skills/{agent_id}/` | Mutable user-Agent Skill authority: immutable digest revisions plus atomic current selectors                    |
| `users/{user_id}/agents/{agent_id}/`               | This user's per-principal Agent Home; sandbox `$HOME` and initial working directory                             |
| `users/group-{group_id}/agents/{agent_id}/`        | This channel group's per-principal Agent Home; sandbox `$HOME` and initial working directory                    |
| `users/{principal}/data/`                          | User or group Principal Home: shared principal data and uploads                                                 |
| `runner-scratch/runner-*`                          | Disposable user-less-run workspace; never durable Home authority                                                |
| `users/{principal}/data/assets/`                   | Uploaded assets; inside the sandbox, use `$STELLA_ASSETS_DIR` rather than an operator path                      |
| `users/{principal}/.mise-tools/`                   | Managed per-user or per-group toolchain; shared by that principal's agents                                      |

## Skills and release bundles

Release-provided builtins are immutable `builtin:<name>` entries from `resources.Registry`. Their only authority is the content-addressed release bundle. Native `local` and `none` execution installs the exact bundle at `$STELLA_HOME/bundles/<revision>`; isolating execution sees that bundle read-only at `/opt/stella/skills/builtin`. `/opt` is only an execution coordinate. Helper executable modes are preserved.

Project Skills remain ordinary files in durable Agent/project working trees. Mutable `system`, `system_agent`, `user`, and `user_agent` current-state metadata and complete file trees are authoritative only in the typed Home roots above. Each current selector names one immutable content-digest revision; PostgreSQL keeps identity, policy, usage, provenance, and migration evidence, not mutable current-state bytes. `system:<name>` is a mutable administrator-installed global Skill, `system_agent:<name>` is a mutable Agent-bound administrator Skill, and neither is a release builtin.

The model never receives a complete mutable Skill authority root. After identity and actor/Agent policy authorization, `skill_load` copies only the selected exact current revision to the active sandbox Session's temporary directory and returns that disposable execution path. Historical, deleted, disabled, deprecated, and other actors' revisions are not part of that view.

Skills are enabled per Agent by default. An administrator or durable Agent creator changes one shared setting. Stella selects the precedence winner before applying that policy, so disabling it never reveals a lower same-name Skill. Activation is independent of content-edit permission and `disable_model_invocation`. An admitted turn keeps its snapshot; the next turn sees a committed change. The database migration canonicalizes historical policy shapes before the strict runtime decoder is used; dangling disabled references are inert until explicitly cleared.

For an exact operator command syntax, run `stellad system-bundle --help`. Docker sandbox images bake and label the matching bundle revision, never fall back to host builtins, and Docker provider preflight prevents a runner session from starting if their revision differs from the binary. Developers rebuild the local image with `mise run sandbox:docker:build`; rebuild custom images from the matching Stella revision.

An upgrade with existing PostgreSQL `skill_file` bytes queues their migration in the background and starts serving unrelated capabilities immediately. Managed Skill reads and writes remain unavailable until cutover succeeds, preventing runtime traffic from racing the legacy inventory; Agent turns continue with release-builtin and Project Skills only. Stella inventories the source twice, quarantines conflicting flat filesystem mirrors, publishes and verifies immutable Home revisions, then atomically records completion and scrubs the legacy bytes. Invalid legacy source data keeps managed Skills disabled and logs the affected Skill and recovery action without blocking unrelated server capabilities; repair the reported data and restart Stella to retry.

`{principal}` is a user ID or `group-{group_id}`. These are deterministic paths
under the single POSIX `STELLA_HOME`, not registry locators. Agents should use their sandbox variables and ordinary relative paths:
`$HOME` for their workspace and `$STELLA_ASSETS_DIR` for uploaded assets. Persistent
XDG state is stored under the principal's `data/` tree; it is not an agent
workspace.

PostgreSQL owner rows authorize workspace access. The sole production
`WorkspaceManager` creates a missing root for live owners and rejects symlinks,
non-directories, unsafe IDs, and replacement of the trusted root. The filesystem
owns the bytes; back it up with PostgreSQL. Any entry at `agents/{id}` reserves
that global Agent ID. Run restore and root cleanup while Stella is stopped.

An explicit destructive user, group, or Agent delete fences execution before the
database transaction removes the owner. Physical bytes and inodes remain, while
subsequent workspace access fails owner validation.
Removing an assignment or member, archiving a Session, and uninstalling Helm do not
delete workspace bytes. Do not manually clean workspace roots while Stella is running.
Multi-replica, Kubernetes, and S3 authority require a future redesign.

`runner-scratch/` is trusted host-owned structural state. Normal close and
construction failure clean each disposable child best-effort; crash or trusted
host tampering may leave children. Isolating providers mount only the exact child.
Clean leftovers only while Stella is stopped or affected consumers are fenced.

## Environment variables

Provider credentials and base URLs are stored in explicit provider rows managed through the Web UI or API; they are not read from the server environment.

| Variable      | Purpose                                     |
| ------------- | ------------------------------------------- |
| `STELLA_HOME` | stella home directory (default `~/.stella`) |

Note: The old YAML-based environment variables (`STELLA_PROVIDER`, `STELLA_MODEL`, `STELLA_TELEGRAM_TOKEN`, etc.) are no longer supported. Use the Web UI or database directly.

## Defaults

On first run, Stella creates one enabled `stella` agent with an empty model and Stella's default system prompt. Provider and channel instances are explicit administrator configuration; built-in plugin capabilities are code-defined and do not require database rows.
