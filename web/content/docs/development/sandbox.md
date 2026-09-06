---
title: Sandbox Backend Abstraction
---

> This section is for developers contributing to Stella. For choosing and configuring a sandbox backend, see the [Sandbox guide](/docs/guides/sandbox).

## Core Model

The sandbox abstraction exists so runner code, plugin wiring, and tool execution do not depend on concrete backend types. Execution always runs through the active backend selected by the runner.

- `pkg/sandbox.Policy` — immutable backend-agnostic execution policy (process-visible filesystem roots, working dir, network mode, env, timeout)
- `pkg/sandbox.Session` — per-run execution boundary and lifecycle owner; combines lifecycle and host-access into one interface
- `pkg/sandbox.FileAccess` — mediated file capability returned by `Session.Files`; callers use the same process-visible coordinates as commands and never receive provider backing paths

Backend identity stays inside the runner and runner-facing sandbox packages. Plugin packages do not import `internal/agent/sandbox`.

## Session Interface

`pkg/sandbox.Session` exposes 8 methods:

| Method                                                     | Description                                                      |
| ---------------------------------------------------------- | ---------------------------------------------------------------- |
| `Policy() Policy`                                          | Returns the immutable policy the session was created with        |
| `Exec(ctx, command, ExecOptions) (ExecResult, error)`      | Run a command and wait for its result                            |
| `StartProcess(ctx, ProcessRequest) (ProcessHandle, error)` | Spawn a long-lived process with stdio handles                    |
| `Files() FileAccess`                                       | Returns mediated access to authorized process-visible data roots |
| `WorkingDir() string`                                      | Returns the logical working directory inside the sandbox         |
| `Close() error`                                            | Tear down the session and release resources                      |
| `Alive() bool`                                             | Reports whether the session is still active                      |
| `Done() <-chan struct{}`                                   | Channel closed when the session terminates                       |

`FileAccess` supports the bounded operations needed by prompt construction and the core `view_image` tool, plus exact-at-publication, no-replace, disposable file projection for managed Skills. A path is relative to `WorkingDir` or absolute in the process view. The public `Policy`, `Session`, and `FileAccess` contracts contain no host-side mount source, path resolver, or path translation result.

Each backend binds the public process roots to a provider-private physical mount plan. File operations use directory capabilities pinned when the Session is created, enforce read-only roots, and fail closed on escapes or cross-mount symlinks. Provider process setup may inspect its private mapping, but no upper layer can obtain a physical path and then bypass the capability with `os.*`.

## Local workspace ownership

Phase 1 supports one replica and one trusted POSIX `STELLA_HOME`. PostgreSQL owner rows are identity and authorization authority; deterministic paths under `STELLA_HOME` are layout and byte authority. `internal/platform/home.WorkspaceManager` is the only production component that creates typed roots. It creates a missing root only after confirming its live user, group, and Agent owners, and rejects symlinks, non-directories, unsafe IDs, and replacement of the trusted root. A user and group with the same raw ID use distinct paths.

A user or group run uses the exact `AgentRoot` and `DataRoot` returned by its authorized `WorkspaceView`. Isolating backends mount those roots read-write; the explicit `none` backend remains trusted-host execution and provides no process-level filesystem isolation. A user-less run retains disposable scratch semantics and receives no principal mount. Group Agent Home Skill materialization has no user or `user_agent` scope: it does not turn group data into a user's `user_agent` Skill.

Outside Session execution, `WorkspaceManager.OpenRoot` mints a scoped read-only or read-write operation capability. Typed root components are materialized with no-follow traversal; operations use an inode-pinned `os.Root`, so contained relative symlinks work while absolute and escaping symlinks fail closed. This is not a `Session` filesystem transport: Stella has no `stella-fs` or Docker-exec filesystem RPC. Downstream file consumers are migrated separately.

Explicit destructive user, group, or Agent deletion fences local execution before deleting the database owner. Files and inodes are retained, but a later `WorkspaceView` fails because the durable owner is gone. Any filesystem entry at `agents/{id}` reserves that global Agent ID. These guarantees are bounded by the trusted host and are single-replica only. Multi-replica deployment keeps the same application model but additionally requires one strongly consistent shared POSIX namespace plus PostgreSQL generation/lease fencing; S3 is not the live workspace authority.

## Current Architecture

### Session ownership

The runner creates a `sandbox.Session` for each run and keeps ownership of its lifecycle. Runner construction fails closed when no active sandbox session is available.

### Backend resolution

The runner resolves the deploy-time backend from `STELLA_SANDBOX_BACKEND` and dispatches through an injected registry of compiled backends. Production deployments use `docker`, `local`, or `none`; the Harbor evaluation harness additionally wires the evaluation-only `bridge` backend.

### Execution-time mediation

All local execution paths that must obey sandbox policy are mediated through the active runner session:

- the core `bash` tool uses `Session.Exec` through the runner-owned session
- the core `view_image` tool and active prompt context reads use `Session.Files`
- managed Skill revisions are copied into an exact, no-replace Session projection through `FileAccess.ProjectFiles`; a conflicting existing tree fails closed
- plugin tools receive `ToolContext.Runtime`, a `pkg/plugins.ToolRuntime` adapter over the active session
- skills and agent preset loading use `ToolRuntime` when running inside an agent session

A core tool that reads files selects one `FileView` per invocation. Its policy environment, working directory, and `FileAccess` come from the same resilient generation, so path expansion cannot silently switch backing trees midway. Provider errors that cross this boundary identify logical process mounts without exposing physical source paths.

A managed Skill projection is atomically published and verified on every load, but it is not a separate isolation boundary from commands running as the same user. Such a command can race verification or modify the disposable tree afterward. A load that observes a mismatch fails closed instead of replacing the path. Session close removes its temporary backing; Docker startup cleanup also removes stale temporary directories left by interrupted sessions.

### Long-lived processes

`Session.StartProcess` is available to backend-owned long-lived processes. MCP
plugin connections remain remote HTTP transports; this interface does not add a
second local MCP execution path.

### Non-runner filesystem access

Some code paths need local filesystem access without an already-injected runtime, such as prompt rendering or metadata discovery outside an active agent run.

Project prompt context and project-scoped Skill reads outside a runner resolve the exact user, Agent, and project, open a read-only Agent Home capability, copy bounded logical content, and close the capability before prompt or Skill processing. They do not treat a logical project `base_dir` as a process working directory. Other trusted non-project metadata discovery may still use the local runtime. These are intentional non-runner paths, not fallbacks for sandboxed tool execution.

### Explicit exception boundary

Remote MCP HTTP/SSE/StreamableHTTP transport is currently treated as a separate trust boundary.

- remote transport dialing is **not** currently mediated by `ToolRuntime`
- this exception is tracked explicitly as `EX-009` and logged as `runtime.exception_path`

## Fail-Closed Behavior

Stella prefers explicit denial over silent downgrade:

- Docker unavailable at session-create time → runner fails to start
- unsupported policies → `PolicyCompatibilityError`, runner fails to start
- direct non-mediated plugin exec → fail closed
- remote MCP HTTP/SSE/StreamableHTTP → explicit exception, not an implicit sandbox bypass

## Verification

The abstraction is covered by:

- session/host contract tests
- policy compatibility tests
- core tool parity tests
- Docker backend integration tests
- static bypass regression guards for migrated runtime paths

## Running the Docker Backend Locally

`mise run dev:docker` brings up the whole stack with one command, mirroring the production `docker-compose.yml`: `stellad` runs **inside a container** with the `docker` sandbox backend in **volume mode** (`STELLA_SANDBOX_BACKEND=docker`, `STELLA_DOCKER_SANDBOX_MODE=volume`, `STELLA_HOME_VOLUME=stella-data`), plus an `otel-lgtm` sidecar. It builds the local images (`docker:build` → `stella:latest`, `sandbox:docker:build` → `stella-sandbox:dev`), creates the named volumes if missing, and ensures `~/.stella-dev/.env` contains a dev vault key. It runs the same `docker-compose.yml` as prod, just exporting `STELLA_IMAGE=stella:latest` so it uses the local build instead of the released image.

The in-container Go server serves its baked-in embedded SPA at `localhost:25688` (see `web/embed.go`), and Grafana is available at `localhost:13413`.

Stop everything with `docker compose down`.

The versioned sandbox image contains the release-owned mise toolchain and
builtin CLI artifacts. Runtime resolves one plugin snapshot from the four
`system`, `system_agent`, `user`, and `user_agent` scopes, then a selection helper
materializes only the chosen entries. Docker preparation is keyed by one
resolved image ID plus the complete selection identity. Native managed installs
use the managed tree; user and user-agent installs stay in their own sandbox
trees and win in `PATH`. No host `_builtin.toml`, manifest permission surface,
or host-platform install is used as a Docker fallback.

## Builtin Skill bundle and projection

`resources.Registry` is the sole authority for release-owned core Skills. It
produces the immutable content-addressed bundle installed at
`$STELLA_HOME/bundles/<revision>` for native `local` and `none` execution.
Isolating execution projects that exact bundle read-only at
`/opt/stella/skills/builtin`; `/opt` is an execution coordinate, not another
authority, and bundle executable helper modes must survive the projection.
Plugin Skills remain owned by their PluginDefinition and selected PluginConfig;
the same four-scope decision gates their exposure. Every builtin plugin can be
disabled through its configuration.

Project Skills remain ordinary files in durable Agent/project working trees and are read through bounded read-only Home snapshots outside active execution. Mutable `system`, `system_agent`, `user`, and `user_agent` identities remain cataloged in PostgreSQL, while their selected revision manifests and bytes are authoritative in durable Home storage. An active Session receives only a disposable, digest-pinned exact projection; revision history never becomes part of the Agent workspace search tree.

The Docker sandbox image bakes and labels the exact core Skill revision. It has
no host-builtin fallback. Docker provider preflight rejects a binary/image
revision mismatch, preventing the runner session from starting. Use
`stellad system-bundle --help` for the operator command syntax. Rebuild the
development image with `mise run sandbox:docker:build`; rebuild every custom
sandbox image from the matching Stella revision.

The cutover is a maintenance upgrade. Stop every old writer before starting the
new runtime, and complete the import and validation in one transaction. Rolling
old and new writers against the same database is not supported.

## Agent Skill policy

Standalone Skills retain their `system`, `system_agent`, `user`, `user_agent`,
and `project` identities plus contextual `builtin` resources. Plugin Skills use
the PluginConfig four-scope model instead of a separate global builtin or
manifest permission surface. Release `builtin:<name>` core resources are
immutable; administrator-installed `system:<name>` and Agent-bound
`system_agent:<name>` standalone Skills are distinct mutable identities.

Resolution selects one winner before policy: `project > user_agent > user > system_agent > system > builtin`. Disabling that winner never exposes a lower same-name Skill. Managed `system:*` and `system_agent:*` policy defaults to enabled, is shared per Agent, and is independent of content-edit authorization and `disable_model_invocation`. Shipped plugin assets use only owner-plugin enablement, not `builtin:*` policy. Project `.agents` skills remain independent. An admitted turn keeps its snapshot; the next turn sees a successful commit. Dangling disabled references have no execution effect and need explicit cleanup.

## Adding a New Backend

Every new sandbox backend requires changes in all of the following locations — missing any one causes a runtime error:

| Step | File                                      | What to do                                                                                                                        |
| ---- | ----------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| 1    | `internal/platform/config/sandbox.go`     | Add the `SandboxBackend<Name> = "<name>"` constant                                                                                |
| 2    | `internal/platform/config/sandbox_env.go` | Accept the name in `ActiveSandboxBackend`'s `STELLA_SANDBOX_BACKEND` switch                                                       |
| 3    | `plugins/sandbox/<name>/session.go`       | Implement `sandbox.Factory` and `sandbox.Session`                                                                                 |
| 4    | `cmd/stellad/setup_sandboxes.go`          | Register a `sandbox.BackendDefinition` adapter and supply any process-owned dependencies                                          |
| 5    | Tests                                     | Cover the backend implementation and composition-root adapter, and keep the dependency guard in `internal/boundary_test.go` green |
| 6    | Docs                                      | Update the [Sandbox guide](/docs/guides/sandbox) and this file                                                                    |

## Related Docs

- [Sandbox guide](/docs/guides/sandbox) — choosing and configuring backends
- [Architecture](/docs/development/architecture)
