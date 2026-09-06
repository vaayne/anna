---
title: CLI design rules
description: Command-line interface conventions for Stella's stellad operator commands.
---

> This is a **rule file** for contributors. When you add or change a `stellad`
> command, read this page first and follow it. Stella follows the spirit of the
> [Command Line Interface Guidelines](https://github.com/cli-guidelines/cli-guidelines):
> make commands predictable, scriptable, composable, and kind to humans using a
> terminal at 2 a.m.

Stella has one binary: `stellad`. It owns server startup, background service
management, upgrades, bootstrap utilities, and database maintenance. It is not a
human chat client and not an agent integration surface. Agent capabilities ship
as native tools via x-agent-tool/toolgen with server-side identity; do not add
sandbox-facing CLI commands, scoped bearer-token shortcuts, or a second client
binary.

The current `stellad` surface is:

- `stellad server` / `stellad serve` — start the server and Web UI.
- `stellad service ...` — install, start, stop, inspect, and uninstall the background service.
- `stellad upgrade` — upgrade the installed daemon binary.
- `stellad version` — print the version.
- `stellad postgres ...` — manage the embedded PostgreSQL runtime.
- `stellad vault keygen` — generate `STELLA_VAULT_KEY` for bootstrap.
- `stellad system-bundle ...` — inspect, install, and verify the builtin Skill bundle.

Before designing command behavior for server-backed features, read [CLI and
native agent tools](../cli-as-client) and [API design rules](./api-design).

## Core Principles

1. **Operator-only scope.** Add `stellad` commands only for bootstrap,
   maintenance, migration, service management, and diagnostics. Product
   features belong in the Web UI, HTTP API, and native agent tools.
2. **Human-friendly by default, machine-friendly on request.** Default output
   should be readable in a terminal. Structured output belongs behind `--json`.
3. **Boring beats clever.** Commands should do exactly what their names imply.
   Avoid hidden behavior, implicit network writes, and surprising defaults.
4. **Composability matters.** Commands should work in scripts: stable exit
   codes, stderr for diagnostics, stdout for data, no unsolicited prompts in
   non-interactive contexts.
5. **Preserve user data.** Destructive commands need explicit intent and clear
   error messages. Never silently delete, overwrite, or revoke.

## Command Shape

Use the project pattern:

```text
stellad <domain> <verb> [args] [flags]
```

Examples:

```text
stellad vault keygen
stellad system-bundle verify
stellad service install --system
```

### Naming

- Top-level commands are daemon domains or lifecycle verbs: `server`, `service`,
  `upgrade`, `version`, `postgres`, `vault`, `system-bundle`.
- Subcommands are verbs or resource nouns when another level is needed:
  `keygen`, `revision`, `install`, `verify`, `status`.
- Use lowercase, hyphen-separated names for multi-word commands and flags.
- Prefer common verbs consistently:

| Action              | Use                                      | Avoid                                     |
| ------------------- | ---------------------------------------- | ----------------------------------------- |
| Show many resources | `list`                                   | `ls`, `all`, `show-all`                   |
| Show one resource   | `get` or `read`                          | `show`, `info` unless already established |
| Create a resource   | `create` or domain-specific `add`/`save` | `new` mixed randomly with `create`        |
| Change a resource   | `update`                                 | `modify`, `edit` unless interactive       |
| Remove a resource   | `delete` or domain-specific `remove`     | `destroy`, `rm`                           |
| Stop running work   | `cancel`                                 | `kill`, `abort`                           |

Keep existing command names stable. If a rename is worth it, keep the old name
as an alias for at least one release unless the old behavior is actively unsafe.

### Arguments vs flags

Use positional arguments for the primary object the command acts on:

```text
stellad upgrade 0.50.0
stellad postgres logs <instance-id>
```

Use flags for modifiers, optional context, filters, and output controls:

```text
stellad service install --system
stellad postgres status --json
```

Rules:

- Required positional args must appear in `ArgsUsage`.
- Required flags are allowed only when there is no natural positional form.
- Boolean flags should be positive (`--follow`, `--json`, `--force`). Avoid
  negative flags like `--no-cache` unless disabling a default is the feature.
- Reuse flag names across commands: `--json`, `--force`, `--install-dir`, `--system`.
- Do not design sandbox-facing CLI commands. Agent capabilities must ship as
  native tools with server-side identity, not CLI flags or scoped bearer tokens.

## Help Text

Every command should be understandable from `stellad help <command>` or
`stellad <command> --help`.

In `urfave/cli` terms:

- `Name`: short, lowercase command token.
- `Usage`: one sentence, imperative or noun-phrase, no trailing period.
- `Description`: only when the command needs context, examples, or warnings.
- `ArgsUsage`: include every positional argument, e.g. `<version>`.
- `Category`: set on top-level commands (`System`, `Admin`) when it helps the
  main help screen.

Good:

```go
Usage:    "Copy a legacy SQLite database into PostgreSQL",
ArgsUsage: "",
```

Bad:

```go
Usage: "Does stuff with the thing",
```

Help text is user-facing documentation. If command names, usage strings, or
important flags change, update user docs. Agent-facing prompts and skills should
point to native tools, not CLI command syntax.

## Output

### stdout is for data

Anything a user may pipe to another program goes to stdout:

```bash
stellad version
```

Human-readable tables and successful result summaries may also go to stdout.
Keep them stable enough that users are not punished for light scripting, but do
not promise table formats as an API.

### stderr is for diagnostics

Progress, warnings, prompts, and errors go to stderr. This keeps stdout clean
for pipes and command substitution.

```text
Downloading release archive...       # stderr
0.50.0                               # stdout
```

### JSON output

Any command expected to be used from scripts should support `--json` unless its
only output is already a raw scalar or file content.

Rules:

- Emit valid JSON to stdout and nothing else on stdout.
- Use the same `snake_case` field names as the API when mirroring API data.
- Prefer the API response shape directly; do not invent a second CLI schema.
- Pretty-printing is fine for human use, but avoid changing the structure.
- Errors still go to stderr and use non-zero exit codes; do not print successful
  JSON envelopes for failed commands.

### Tables

For human list output, use aligned columns with concise headers. Avoid wrapping
large text in tables; put long content in `read`, `get`, or `--json` output.

Use stable identifiers in the first column when possible:

```text
ID        STATUS    TITLE
abc123    open      Fix scheduler retry
```

## Errors and Exit Codes

Errors should be brief, specific, and actionable:

```text
connect to Stella server: connection refused (start it with `stellad server`)
```

Bad errors leak implementation or force guesswork:

```text
sql: no rows in result set
invalid input
```

Exit code rules:

| Code | Meaning                                                             |
| ---- | ------------------------------------------------------------------- |
| 0    | Success                                                             |
| 1    | Expected failure: validation, not found, server error, auth failure |
| 2    | Command-line usage error when the CLI framework distinguishes it    |

Do not add a complex exit-code taxonomy unless a real automation use case needs
it. One bit of failure is enough most of the time.

When wrapping errors in Go, add the operation context once:

```go
return fmt.Errorf("system-bundle install: %w", err)
```

Do not wrap the same noun at every layer until the message reads like a haunted
stack trace.

## Interactivity

Interactive behavior is allowed only when stdin is a terminal and the command is
clearly user-driven.

Rules:

- Detect non-interactive use before prompting.
- Provide flags for scripted use instead of requiring prompts.
- Confirmation prompts for destructive actions should show what will be changed.
- `--force` may skip confirmation, but it must not widen the operation.
- Never prompt for secrets if the value can be read from a standard environment
  variable or generated as bootstrap output.

## Destructive Commands

A destructive command deletes, revokes, overwrites, cancels, archives, or sends
something externally visible.

Requirements:

1. The command name must make the action obvious: `delete`, `remove`, `cancel`,
   `revoke`, `send`.
2. Target selection must be explicit. Avoid destructive commands that operate on
   an implicit "current" resource.
3. Bulk destructive operations need either a narrow filter plus confirmation or
   an explicit `--force`.
4. Dry-run support is preferred for broad operations.

Do not make `stellad <thing> sync` delete remote data unless the help text and
confirmation say that plainly. Surprise deletion is how tools get uninstalled
with prejudice.

## Configuration and Environment

Use configuration precedence consistently:

```text
flag > environment variable > persisted config > default
```

Document environment variables in help text when they affect behavior. Common
variables include:

| Variable              | Purpose                                    |
| --------------------- | ------------------------------------------ |
| `STELLA_HOME`         | Stella home directory                      |
| `STELLA_DATABASE_URL` | External PostgreSQL connection URL         |
| `STELLA_VAULT_KEY`    | age secret key for daemon vault encryption |
| `LOG_LEVEL`           | CLI logging verbosity (default `INFO`)     |
| `LOG_LEVEL_RIVER`     | River job-queue logging (default `WARN`)   |

Never print secrets except for explicit generation commands such as
`stellad vault keygen`, where the secret value is the requested output.

## Server Access

A `stellad` command that needs server state should prefer calling the same
internal service layer used by `stellad server`, or the generated API client when
it is intentionally operating against a running server. It must not duplicate
business logic in a second command path.

Pattern:

1. Build a typed request from args and flags.
2. Call the service layer or generated client.
3. Render the response.
4. Return errors with command context.

If the server is required but unavailable, say how to fix it:

```text
connect to Stella server: connection refused (start it with `stellad server`)
```

## Logging and Verbosity

- Normal successful commands should be quiet.
- Progress belongs on stderr only when the operation takes noticeable time.
- Debug logs should be controlled by `LOG_LEVEL` and must not pollute stdout.
- Do not log request bodies containing secrets, tokens, email content, or user
  prompts unless explicitly redacted.

## Compatibility

CLI users script everything. Treat command names, flag names, JSON fields, and
exit behavior as compatibility surfaces.

- Add flags instead of changing existing flag meaning.
- Keep old aliases when renaming commands.
- Avoid changing default output order if users plausibly pipe it.
- Prefer additive JSON fields; do not change field types.
- Remove behavior only after checking docs, tests, and known callers.

**Pre-launch exception.** Stella has not shipped a stable release yet, so there
are no external scripts to protect. Until the first release, prefer full
conformance over compatibility shims: rename commands to the correct shape and
drop legacy aliases outright instead of keeping them around. After launch, the
compatibility rules above apply in full.

## Implementation Checklist

When adding or changing a `stellad` command:

1. Is this truly an operator/bootstrap/maintenance command rather than a product
   feature that belongs in the Web UI, API, or native agent tool?
2. Does the command follow `stellad <domain> <verb>` and existing domain naming?
3. Are primary targets positional and modifiers flags?
4. Are `Usage`, `Description`, and `ArgsUsage` clear in `stellad help`?
5. Does stdout contain only command data, with diagnostics on stderr?
6. Does scriptable output support `--json` where appropriate?
7. Are errors actionable and wrapped with useful command context?
8. Are destructive actions explicit and protected from accidental broad impact?
9. Does config precedence follow `flag > env > config > default`?
10. Are docs and `internal/agent/prompt/template/system_prompt.tmpl` updated if
    command usage changed?
11. Are command tests updated for args, flags, output, and error behavior?
