# stella

Stella is a single-tenant, multi-user, multi-agent AI assistant platform written in Go. Each deployment is one tenant and trust boundary. It pairs each user with personalized AI agents that have their own memory, tools, schedules, and sandbox policies. Users interact through Telegram, Discord, QQ, Feishu, WeChat, or the Web UI. The backend is a single `stellad` binary backed by PostgreSQL (an embedded cluster by default, or an external server via `STELLA_DATABASE_URL`).

## Layout

- `cmd/stellad/` is the single server binary entry point and owns operator commands, service management, and startup wiring.
- `internal/` is the Go backend (~36 packages). Agents usually need `agent` (runtime/sandbox/tools), `server` (HTTP API), `auth`/`authz`, `db` (migrations and sqlc queries), `scheduler`, and `goal`.
- `internal/platform/cli/` is shared command-output/env plumbing for `stellad`, not a user-facing chat CLI.
- `api/` contains the OpenAPI spec and generated contracts; follow `api/CLAUDE.md` for spec-first API changes.
- `web/` contains the frontend and docs content, including these development rules.
- `plugins/` contains built-in plugin packages and their shipped skills.
- `resources/` contains other embedded runtime resources; `pkg/` contains reusable public Go packages.

## Commands

- This project requires `mise` for development workflows.
- On a fresh clone, run `mise run setup` once. Use `mise tasks` to discover workflows.
- Run project workflows through `mise run <task>` instead of invoking underlying tools directly.
- A task whose body is a script lives in `.mise/tasks/` (path = task name); one-liners stay in `mise.toml`. File tasks run under `set -euo pipefail`, must work on bash 3.2 (what macOS ships), and are linted by `mise run lint:shell`, which `mise run format` includes.
- Before committing, **ALWAYS** run: `mise run format && mise run build && mise run test`.
- `mise run test:e2e` runs all functional Playwright specs in `test/e2e/`; use `mise run test:e2e -- --grep-invert @model` for a fast run without real-model turns. `mise run perf -- <label>` runs render and load performance measurements.
- `mise run test` runs Go tests, frontend unit tests, and the subprocess system suite (real `stellad` over TCP against embedded PostgreSQL); the system portion requires a supported runtime host. `mise run release:validate` runs the full local pre-release gate sequentially (format → build → test → release checks).
- When touching platform-specific behavior, run a targeted cross-platform build before committing (e.g., `GOOS=windows GOARCH=amd64 go build -o dist/bin/stellad-windows-amd64.exe ./cmd/stellad`).
- Do not run Go tests with `-race` locally by default.
- Build with `mise run build` (outputs to `dist/bin/`) or specify `-o dist/bin/stellad` explicitly; never build the `stellad` binary into the repo root.
- `mise run dev` writes combined UI/API output to `dist/logs/dev.log` and truncates that file on each startup; use it for agent-friendly debugging.
- For a fresh agent-driven UI/API test instance, use only `mise run testbed:start` and `mise run testbed:stop`. Start is long-running: run it in a dedicated terminal or background task, read the printed credentials path without displaying its contents, then let stop own all cleanup. Never use `~/.stella-dev`, manual fixtures, or browser/CDP registration for these tests; use browser registration only when registration itself is under test. See `testing.md`.

## Goose migrations

- Create migrations only with `mise run db:migrate:new -- <name>`; after
  `90000000000000_sequential_versioning.sql`, versions are sequential and
  contiguous. Never run `goose fix`.

## Development rules

Rules in `web/content/docs/development/rules/` are the **source of truth** for development conventions. Read the relevant rule before designing or changing anything in that domain. Bare filenames below live in that directory; a row that names a rule kept elsewhere gives its full path.

| Domain            | Rule file                     | Read before                                                                 |
| ----------------- | ----------------------------- | --------------------------------------------------------------------------- |
| Schema design     | `schema-design.md`            | Designing or changing any table                                             |
| goose migrations  | `goose.md`                    | Creating or modifying database migrations                                   |
| sqlc queries      | `sqlc.md`                     | Writing or editing SQL query files                                          |
| API design        | `api-design.md`               | Designing or changing any HTTP API                                          |
| Agent tools       | `agent-tools.md`              | Adding, changing, renaming, or removing any model-facing tool               |
| Go patterns       | `go-patterns.md`              | Writing or reviewing Go concurrency, secret-redaction, or file-install code |
| CLI design        | `cli-design.md`               | Designing or changing any `stellad` operator command                        |
| Bundled runtimes  | `bundled-runtimes.md`         | Embedding a third-party CLI in `stellad` or changing `$STELLA_HOME/bin`     |
| Web UI            | `web-ui.md`                   | Building or reviewing any web UI                                            |
| Web theming       | `web-theming.md`              | Changing the web visual style or tokens                                     |
| Current web theme | `web-design.md`               | Styling against the current theme or consulting the visual direction        |
| Documentation     | `doc-style.md`                | Writing or editing user/developer docs                                      |
| Testing           | `testing.md`                  | Running, adding, or choosing a layer for any test                           |
| Project tracking  | `project-tracker.md`          | Managing Feishu plans, GitHub issues, and pull requests                     |
| Release           | `release.md`                  | Cutting a release, tagging, changelog                                       |
| Eval loop         | `test/evals/harbor/README.md` | Measuring any change to agent behavior (tools, prompts, runner loop)        |
| Marketing         | `marketing.md`                | Writing a landing page, README opener, hero copy, or any marketing content  |

For new or changed HTTP APIs, also follow `api/CLAUDE.md` for the OpenAPI-first workflow.

Test at the lowest sufficient layer: keep deterministic behavior in package Go tests, add a `test/system` journey only for a process seam a Go test cannot reach, and put browser/API/perf flows in `test/e2e`. See `testing.md`.

## Measuring a change

Agent-behavior changes (tools, prompts, the runner loop) are measured against Terminal-Bench through the Harbor eval loop, not argued from first principles.

- **Read `test/evals/harbor/README.md`** before running or citing an eval; its "Evaluating a change" section is the procedure and `PROTOCOL.md` is the authority on what a comparison may conclude.
- Take the quick and full references **before** you change anything, on the same machine and model, from the commit you branched off. Iterate on `--tier quick` (~5 min), then confirm at single-task `k=5` with `--confirm` — that pair runs candidate first, reference second, per `PROTOCOL.md`. Run the full tier on both sides before opening the PR.
- A rise at loop k is a `SIGNAL`, not an improvement. Only a `--confirm` verdict backs an improvement claim in a PR.
- Put the evidence in the PR as a table naming both jobs, commits, tier, k, host, and model. If a change touches a surface no task exercises (images, documents, CRLF, binaries, non-UTF-8), say so rather than letting the score imply coverage.
- Never set `OTEL_STELLA_RECORD_TOOL_IO` for an eval run: Terminal-Bench ships synthetic-secret tasks.

## Timestamps and timezones

- **Store UTC and serialize timezone-aware.** Use `time.Now().UTC()` in Go; schema defaults use `now()`. Emit RFC3339 with zone via `t.UTC().Format(time.RFC3339)`. Never return naive local time in code or API responses. pgx scans `timestamptz` into `time.Time`; call `.UTC()` before formatting.
- Convert to the user's local zone only at the presentation layer.

## Documentation

Read `web/content/docs/development/rules/doc-style.md` before writing or editing any doc.

When behavior, APIs, config, commands, or architecture change:

- Update `README.md` and/or `web/content/docs/` as appropriate.
- Keep `plugins/core/skills/stella/` and `internal/agent/prompt/template/system_prompt.tmpl` in sync with user-facing changes.
- Maintain both English (`*.md`, `*.mdx`) and Chinese (`*.zh.md`, `*.zh.mdx`) versions.

## Issue & PR tracking

**Read `web/content/docs/development/rules/project-tracker.md`** for the full workflow. Its templates are the contract; never compose an issue or PR body from memory.

**When an issue is needed.** Most PRs link one. A small self-contained fix (typo, one-line bug, doc correction) can go straight to a PR with `No issue: <reason>` in Refs. Anything that needs discussion, changes external behavior, or spans several areas gets an issue first, created on the user's behalf if needed. A Feishu Task is not a prerequisite: committed Feishu Tasks create their own issue, and issues without a task are reconciled by the Tuesday delivery review.

**Writing an issue.** Maintainer-created implementation issues use `What`, `Why`, `How`, `Refs`. `.github/ISSUE_TEMPLATE/*.yml` are GitHub _forms_: `gh issue create --body` bypasses them entirely, so read the matching form and render its required fields as those markdown headings yourself. The form also carries the pieces the CLI will not apply for you — the title prefix and the labels. Always set the type label (`bug`, `enhancement`, `documentation`, …) and the status label (`status:accepted` when accepted but unscheduled, `status:ready` when committed and not started, none when a PR is already open). Never add a priority label; priority lives in Feishu.

**Writing a PR.** The body follows `.github/pull_request_template.md`. Read that file, write the body into a temp file, then `gh pr create --body-file` / `gh pr edit --body-file`. Keep its exact headings (`What`, `Why`, `How`, `Test`, `Refs`, `Checklist`) and answer every checklist item, marking the ones that do not apply with the reason instead of dropping them.

**Keep the record honest as it changes.** An issue or PR that grew past its original scope gets its title, body, and labels rewritten to match what it now covers, not left describing its first commit.

## Commit style

Emoji-prefixed Conventional Commits, e.g. `✨ feat:` / `🐛 fix:`. No `Signed-off-by` unless requested.
