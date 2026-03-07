## Project Overview

anna is a minimal Go CLI that acts as a local AI assistant. Native Go runner calling LLM providers directly. Two interfaces: interactive CLI chat and Telegram bot (long polling).

## Architecture

```
main.go                         → Entry point, signal handling, wiring
config.go                       → Config types, YAML loading, env var overrides
agent/runner/runner.go          → Runner interface, Event, RPCEvent, HandlerFunc
agent/runner/go/runner.go       → Go runner: native LLM provider calls
agent/pool.go                   → Pool: session management, history, runner lifecycle
agent/session.go                → Session: event history + runner
channel/notifier.go             → Notifier/Backend interfaces, Dispatcher
channel/telegram/telegram.go    → Telegram bot: core struct, lifecycle, guard
channel/telegram/handler.go     → Command handlers, text handling
channel/telegram/model.go       → Model selection keyboard, pagination
channel/telegram/stream.go      → Streaming (draft + edit), tool display
channel/telegram/render.go      → Markdown rendering, message splitting
channel/cli/cli.go              → Interactive terminal chat
channel/cli/chat.go             → Bubble Tea TUI chat model
cron/cron.go                    → Scheduled jobs (gocron/v2)
memory/                         → Persistent memory (facts, journal)
```

## Development

```bash
mise run build      # Build → bin/anna
mise run test       # Tests with -race
mise run lint       # go vet
mise run format     # gofmt + go mod tidy
```

Config: `.agents/config.yaml` | Sessions: `.agents/workspace/sessions`

## Go Code Standards

Write idiomatic Go. Follow these strictly:

**Concurrency safety**
- Protect shared state with `sync.Mutex` or `sync.RWMutex`. Never access maps concurrently without synchronization.
- Prefer passing values through channels over sharing mutable state.

**Error handling**
- Always propagate errors to callers. Never silently swallow errors in functions that return `error`.
- Wrap errors with `fmt.Errorf("context: %w", err)` for traceability.

**File organization**
- Split files by responsibility. One file should not exceed ~300 lines.
- Group related types, methods, and helpers in the same file.

**API design**
- Return errors, don't just log them. Let the caller decide how to handle failures.
- Use named return values sparingly — only when it genuinely improves clarity.

**Avoid these anti-patterns**
- Stringly-typed code: use constants or typed enums, not raw string literals for modes/statuses.
- Duplicate logic: extract shared patterns into helpers rather than copy-pasting with slight variation.
- Dead code: don't leave redundant checks that are already handled by a caller/middleware.
- Byte-offset slicing on strings: use `unicode/utf8` for safe truncation of user-facing text.
- Orphaned comments: don't leave doc comments for declarations that don't exist.
- Storing `context.Context` in structs: pass it through function parameters instead (acceptable only when required by framework constraints like long-polling bots).

**Testing**
- Tests in `_test.go` files, same package. Mock runners for agent tests.
- Run with `-race`. Target >80% coverage.

## Conventions

- Package structure: `main`, `agent`, `agent/runner`, `channel/cli`, `channel/telegram`, `cron`, `memory`
- Telegram bot via `gopkg.in/telebot.v4` (long polling, raw API for unsupported methods)
- Conventional commits with emoji prefixes: `✨ feat:`, `🐛 fix:`, `♻️ refactor:`, `📝 docs:`
