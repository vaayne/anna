## Project Overview

anna is a minimal Go CLI that acts as a local AI assistant. It uses a native Go runner that calls LLM providers directly. Two interfaces: interactive CLI chat and Telegram bot (long polling).

## Architecture

```
main.go                         → Entry point, signal handling, wiring
config.go                       → Config types, YAML loading, env var overrides
agent/runner/runner.go          → Runner interface, Event, RPCEvent, HandlerFunc, optional interfaces
agent/runner/go/runner.go       → Go runner: native LLM provider calls
agent/pool.go                   → Pool: session management, history, runner lifecycle
agent/session.go                → Session: event history + runner
channel/notifier.go             → Notifier/Backend interfaces, Dispatcher (multi-backend routing)
channel/notify_tool.go          → Notify agent tool (send messages via dispatcher)
channel/telegram/telegram.go    → Telegram bot: long polling, notifications, group support
channel/cli/cli.go              → Interactive terminal chat
channel/cli/chat.go             → Bubble Tea TUI chat model
cron/cron.go                    → Scheduled jobs (gocron/v2)
cron/tool.go                    → Cron agent tool
memory/                         → Persistent memory (facts, journal)
```

## Development

Uses [mise](https://mise.jdx.dev/) for task automation.

```bash
mise tasks          # List all tasks
mise run build      # Build binary → bin/anna
mise run test       # Run tests with race detection
mise run lint       # go vet
mise run format     # gofmt + go mod tidy
mise run run:chat   # Build + run CLI chat
mise run run:gateway  # Build + run gateway daemon
```

## Configuration

Config file: `.agents/config.yaml`
Session data: `.agents/workspace/sessions`

Env var overrides:
- `ANNA_TELEGRAM_TOKEN` → telegram.token
- `ANNA_TELEGRAM_NOTIFY_CHAT` → telegram.notify_chat
- `ANNA_TELEGRAM_CHANNEL_ID` → telegram.channel_id
- `ANNA_TELEGRAM_GROUP_MODE` → telegram.group_mode
- `ANNA_RUNNER_TYPE` → runner.type
- `ANNA_PROVIDER` → provider
- `ANNA_MODEL` → model
- `ANNA_MODEL_STRONG` → models.strong
- `ANNA_MODEL_WORKER` → models.worker
- `ANNA_MODEL_FAST` → models.fast

### Telegram Config

```yaml
telegram:
  token: "BOT_TOKEN"
  notify_chat: "123456789"    # chat ID for proactive notifications
  channel_id: "@my_channel"   # optional broadcast channel
  group_mode: "mention"       # mention | always | disabled
  allowed_ids:                # user IDs allowed to use the bot (empty = allow all)
    - 136345060
```

### Notification System

Multi-backend dispatcher routes notifications to all registered backends (or a specific one).

- `channel.Backend` interface: `Name() string` + `Notify(ctx, Notification) error`
- `channel.Dispatcher`: register backends, broadcast or route by channel name
- `notify` agent tool: LLM can proactively push messages via any backend
- Cron results are broadcast to all backends via the dispatcher

Adding a new backend (Slack, Discord, etc.): implement `Backend`, register with dispatcher in `runGateway()`.

## Code Conventions

- Channel-based package structure: `main`, `agent`, `agent/runner`, `agent/runner/go`, `channel/cli`, `channel/telegram`
- Telegram bot via `gopkg.in/telebot.v4` (long polling)
- Conventional commits with emoji prefixes

## Testing

- Tests use `_test.go` files in each package
- Agent tests use mock runners for unit testing
- Config tests use temp directories
- Target: >80% coverage
- Run: `mise run test`
