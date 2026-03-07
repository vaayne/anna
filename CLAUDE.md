## Overview

anna is a Go CLI local AI assistant. Native Go runner calling LLM providers. Two interfaces: CLI chat (Bubble Tea TUI) and Telegram bot (long polling via `gopkg.in/telebot.v4`).

## Packages

`main` → `agent/` (pool, session) → `agent/runner/` (GoRunner, engine loop) → `ai/` (providers, types, stream)

Side packages: `channel/` (cli, telegram, notifier) → `cron/` → `memory/` → `agent/tool/`

Config: `~/.anna/config.yaml` | Sessions: `~/.anna/workspace/sessions`

## Tasks

```bash
mise run build    # → bin/anna
mise run test     # -race
mise run lint     # go vet
mise run format   # gofmt + go mod tidy
```

## Rules

- Protect shared state with mutexes. Never access maps concurrently without sync.
- Always propagate errors. Never silently swallow them.
- Split files by responsibility, ~300 lines max per file.
- No stringly-typed code — use constants for modes/statuses.
- No duplicate logic — extract shared patterns into helpers.
- No dead code or orphaned comments.
- Use `unicode/utf8` for safe string truncation.
- Conventional commits: `✨ feat:`, `🐛 fix:`, `♻️ refactor:`, `📝 docs:`
- Tests with `-race`, >80% coverage.

## Documentation

After completing any task that changes behavior, APIs, config, CLI commands, or architecture:

1. Check if `README.md` or any file in `docs/` needs updating.
2. Update affected docs to match the new code. Keep README concise -- detailed content belongs in `docs/`.
3. If adding a new subsystem, create a new doc in `docs/` and link it from the README documentation table.

Docs structure:
- `README.md` -- Quick start, feature list, architecture overview, links to docs
- `docs/configuration.md` -- Full config YAML reference, env vars, defaults
- `docs/architecture.md` -- System design, package layout, providers, tools
- `docs/telegram.md` -- Bot setup, streaming, groups, access control
- `docs/models.md` -- Tiers, CLI commands, provider setup, caching
- `docs/memory-system.md` -- Facts + journal, tool interface
- `docs/cron-system.md` -- Scheduled tasks, job persistence
- `docs/session-compaction.md` -- History compaction, token management
- `docs/notification-system.md` -- Dispatcher, backends, agent tool

## Release

See `.agents/skills/release/SKILL.md` for the full release workflow. Quick ref:

```bash
mise run release:check     # Validate config
mise run release:snapshot  # Test release locally
mise run release           # Production release (requires tag)
```

Tag format: `vX.Y.Z` (semver). Push tag to trigger CI.
