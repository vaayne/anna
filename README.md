# anna

A minimal Go CLI that acts as a local AI assistant. Uses a native Go runner that calls LLM providers (Anthropic, OpenAI) directly.

Two interfaces: **interactive CLI chat** and **gateway daemon** (Telegram bot via long polling).

## Features

- Native Go runner calling LLM providers directly (Anthropic, OpenAI)
- Interactive CLI chat with Bubble Tea TUI and streaming responses
- Telegram bot via long polling (no webhook, no public IP needed)
- **Notification system** — multi-backend dispatcher for proactive messaging
  - Agent `notify` tool — LLM can push messages to users
  - Cron job results broadcast to all notification backends
  - Extensible: add Slack, Discord, etc. by implementing the `Backend` interface
- **Telegram group support** — configurable `group_mode` (mention/always/disabled)
- **Access control** — `allowed_ids` restricts bot to specific Telegram users
- Tiered model config (strong/worker/fast) with runtime model switching
- Per-chat session management with persistent history (JSONL)
- Session compaction with LLM-generated summaries
- Scheduled tasks via cron with persistent job storage
- Persistent memory (facts + journal)
- Idle runner auto-reaping (configurable timeout)
- Graceful shutdown on SIGINT/SIGTERM

## Prerequisites

- Go 1.24+
- An API key for at least one LLM provider (Anthropic, OpenAI)
- (Optional) [mise](https://mise.jdx.dev/) for task automation

## Install

```bash
go install github.com/vaayne/anna@latest
```

Or build from source:

```bash
git clone https://github.com/vaayne/anna.git
cd anna
go build -o anna .
```

## Usage

### CLI Chat

```bash
anna chat
```

Starts an interactive terminal session with streaming responses.

### Gateway (Daemon)

```bash
anna gateway
```

Starts all configured services (Telegram bot, cron scheduler). Services are activated based on config — e.g., Telegram starts only when a token is provided.

## Configuration

Config file: `.agents/config.yaml`

```yaml
provider: anthropic
model: claude-sonnet-4-6

# Tiered models (optional)
models:
  strong: claude-sonnet-4-6
  worker: claude-haiku-4-5
  fast: claude-haiku-4-5

# Provider credentials
providers:
  anthropic:
    api_key: "sk-..."
  openai:
    api_key: "sk-..."
    base_url: "https://api.openai.com/v1"

# Runner settings
runner:
  type: go
  idle_timeout: 10          # minutes before reaping idle runners
  compaction:
    max_tokens: 80000       # auto-compact when history exceeds this
    keep_tail: 20           # keep N recent messages after compaction

# Telegram bot
telegram:
  token: "BOT_TOKEN"
  notify_chat: "123456789"  # chat ID for proactive notifications
  channel_id: "@my_channel" # optional broadcast channel
  group_mode: "mention"     # mention | always | disabled
  allowed_ids:              # restrict to these user IDs (empty = allow all)
    - 136345060

# Scheduled tasks
cron:
  enabled: true

# Session persistence directory
sessions: ".agents/workspace/sessions"
```

### Environment Variable Overrides

| Variable | Overrides |
|----------|-----------|
| `ANNA_PROVIDER` | `provider` |
| `ANNA_MODEL` | `model` |
| `ANNA_MODEL_STRONG` | `models.strong` |
| `ANNA_MODEL_WORKER` | `models.worker` |
| `ANNA_MODEL_FAST` | `models.fast` |
| `ANNA_RUNNER_TYPE` | `runner.type` |
| `ANNA_TELEGRAM_TOKEN` | `telegram.token` |
| `ANNA_TELEGRAM_NOTIFY_CHAT` | `telegram.notify_chat` |
| `ANNA_TELEGRAM_CHANNEL_ID` | `telegram.channel_id` |
| `ANNA_TELEGRAM_GROUP_MODE` | `telegram.group_mode` |
| `ANTHROPIC_API_KEY` | `providers.anthropic.api_key` |
| `OPENAI_API_KEY` | `providers.openai.api_key` |

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                        anna                             │
│                                                         │
│  ┌──────────┐      ┌────────────────┐                  │
│  │ CLI Chat  │─────▶│                │                  │
│  └──────────┘      │     Pool       │   LLM Providers  │
│                     │ (sessions +   │◄────────────────▶ Anthropic / OpenAI
│  ┌──────────┐      │  Go runner)   │   HTTP API        │
│  │ Telegram  │─────▶│                │                  │
│  │ LongPoll  │      └───────┬────────┘                  │
│  └──────────┘              │                            │
│                     ┌──────▼────────┐                   │
│  ┌──────────┐      │  Dispatcher   │                   │
│  │   Cron   │─────▶│ (notify tool) │──▶ Telegram       │
│  └──────────┘      │               │──▶ Slack (future) │
│                     └───────────────┘                   │
└─────────────────────────────────────────────────────────┘
```

```
main.go                         Entry point, signal handling, wiring
config.go                       Config types, YAML loading, env var overrides
agent/pool.go                   Session management, history, runner lifecycle
agent/runner/go/runner.go       Go runner: native LLM provider calls
channel/notifier.go             Notification dispatcher (multi-backend)
channel/notify_tool.go          Agent notify tool
channel/telegram/telegram.go    Telegram bot + notification backend
channel/cli/cli.go              Interactive terminal chat
cron/cron.go                    Scheduled jobs with gocron/v2
memory/                         Persistent memory (facts + journal)
```

## Development

Uses [mise](https://mise.jdx.dev/) for task automation:

```bash
mise run build          # Build binary → bin/anna
mise run test           # Run tests with race detection
mise run lint           # go vet
mise run format         # gofmt + go mod tidy
mise run run:chat       # Build + run CLI chat
mise run run:gateway    # Build + run gateway daemon
```

Or with plain Go:

```bash
go build -o anna .
go test -race ./...
```

## License

MIT
