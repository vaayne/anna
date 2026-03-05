# pibot

A minimal Go CLI that acts as a local AI assistant. It spawns [Pi](https://github.com/anthropics/claude-code) as a local process and communicates via JSON-RPC over stdin/stdout.

Two interfaces: **interactive CLI chat** and **Telegram bot** (long polling).

~880 lines of Go. Single dependency (`gopkg.in/yaml.v3`).

## Features

- Spawn and manage local Pi processes via JSON-RPC over stdin/stdout
- Interactive CLI chat mode with streaming responses
- Telegram bot via long polling (no webhook, no public IP needed)
- Per-chat session management using Pi's native session files
- Idle process auto-reaping (configurable timeout, default 10min)
- Graceful shutdown on SIGINT/SIGTERM
- Telegram message auto-splitting at newline boundaries (4000 char limit)
- Crash detection with auto-respawn on next message

## Prerequisites

- Go 1.23+
- [Pi](https://github.com/anthropics/claude-code) installed and on your PATH
- (Optional) [mise](https://mise.jdx.dev/) for task automation

## Install

```bash
go install github.com/vaayne/pibot@latest
```

Or build from source:

```bash
git clone https://github.com/vaayne/pibot.git
cd pibot
go build -o pibot .
```

## Usage

### CLI Chat

```bash
pibot chat
```

Starts an interactive terminal session. Type your message, get streaming responses. `/quit` or `/exit` to stop.

### Telegram Bot

```bash
# Via config file
pibot telegram

# Or via environment variable
PIBOT_TELEGRAM_TOKEN=your-token pibot telegram
```

Get a bot token from [@BotFather](https://t.me/BotFather) on Telegram.

## Configuration

Config file: `~/.pibot/config.yaml`

```yaml
# Pi CLI configuration
pi:
  # Path to the pi binary (default: "pi")
  binary: "pi"
  # Minutes of inactivity before closing a session (default: 10)
  idle_timeout: 10

# Telegram bot configuration
telegram:
  # Bot token from @BotFather
  token: "YOUR_TELEGRAM_BOT_TOKEN"

# Directory for session state files (default: ~/.pibot/sessions)
sessions: "~/.pibot/sessions"
```

### Environment Variable Overrides

| Variable | Overrides |
|----------|-----------|
| `PIBOT_TELEGRAM_TOKEN` | `telegram.token` |
| `PIBOT_PI_BINARY` | `pi.binary` |

## Architecture

```
┌───────────────────────────────────────────────────────┐
│                      pibot                             │
│                                                        │
│  ┌──────────┐      ┌────────────────┐                │
│  │ CLI Chat  │─────▶│                │                │
│  └──────────┘      │ SessionManager │  stdin/stdout   │
│                     │ (agent pool +  │◄──────────────▶ Pi Process(es)
│  ┌──────────┐      │  idle timeout) │  JSON-RPC       │
│  │ Telegram  │─────▶│                │                │
│  │ LongPoll  │      └────────────────┘                │
│  └──────────┘                                         │
└───────────────────────────────────────────────────────┘
```

```
main.go            Entry point, signal handling, wiring
config.go          Config types, YAML loading, env var overrides
agent/agent.go     Pi process lifecycle, JSON-RPC protocol
agent/session.go   Agent pool by session ID, idle reaping
bot/telegram.go    Telegram long polling, message splitting
cli/chat.go        Interactive terminal chat
```

## Development

Uses [mise](https://mise.jdx.dev/) for task automation:

```bash
mise run build          # Build binary → bin/pibot
mise run test           # Run tests with race detection
mise run lint           # go vet
mise run format         # gofmt + go mod tidy
mise run run:chat       # Build + run CLI chat
mise run run:telegram   # Build + run Telegram bot
mise run clean          # Remove build artifacts
```

Or with plain Go:

```bash
go build -o pibot .
go test -race ./...
```

Test coverage: **83.5%**

## License

MIT
