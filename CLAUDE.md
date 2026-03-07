## Overview

anna is a Go CLI local AI assistant. Native Go runner calling LLM providers. Two interfaces: CLI chat (Bubble Tea TUI) and Telegram bot (long polling via `gopkg.in/telebot.v4`).

## Packages

`main` → `agent/` (pool, runner, session) → `channel/` (cli, telegram, notifier) → `cron/` → `memory/`

Config: `.agents/config.yaml` | Sessions: `.agents/workspace/sessions`

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
