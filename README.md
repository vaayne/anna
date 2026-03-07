# anna

A minimal Go CLI that acts as a local AI assistant. Uses a native Go runner that calls LLM providers (Anthropic, OpenAI, OpenAI-compatible) directly.

Two interfaces: **interactive CLI chat** and **gateway daemon** (Telegram bot via long polling).

## Features

- Native Go runner calling LLM providers directly (Anthropic, OpenAI, OpenAI-compatible)
- Interactive CLI chat with Bubble Tea TUI and streaming responses
- Telegram bot via long polling (no webhook, no public IP needed)
  - Streaming drafts (Bot API 9.3+) for smooth animated responses
  - Group support with configurable `group_mode` (mention/always/disabled)
  - Access control via `allowed_ids`
- Notification system with multi-backend dispatcher
- Model management CLI (`anna models list/update/set/search`)
- Tiered model config (strong/worker/fast) with runtime model switching
- Per-chat session management with persistent history (JSONL)
- Session compaction with LLM-generated summaries
- Scheduled tasks via cron with persistent job storage
- Skill management (search, install, list, remove from [skills.sh](https://skills.sh) ecosystem)
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

## Quick Start

### CLI Chat

```bash
anna chat            # Interactive TUI
anna chat --stream   # Pipe prompt via stdin, stream to stdout
```

### Gateway (Daemon)

```bash
anna gateway
```

Starts all configured services (Telegram bot, cron scheduler). Services are activated based on config.

### Model Management

```bash
anna models             # List available models (alias for list)
anna models list        # List all models grouped by provider
anna models update      # Fetch models from provider APIs and update cache
anna models current     # Show active provider/model
anna models set <p/m>   # Switch model (e.g. anna models set openai/gpt-4o)
anna models search <q>  # Search models by name
```

### Skill Management

```bash
anna skills              # List installed skills (alias for list)
anna skills list         # List installed skills grouped by source
anna skills list --json  # List as JSON
anna skills search <q>   # Search skills.sh ecosystem
anna skills install <s>  # Install (e.g. anna skills install owner/repo@skill-name)
anna skills remove <n>   # Remove an installed skill
```

## Configuration

Config file: `~/.anna/config.yaml` -- see [docs/configuration.md](docs/configuration.md) for full reference.

The config directory defaults to `~/.anna` and can be changed via the `ANNA_HOME` environment variable.

Minimal example to get started:

```yaml
providers:
  anthropic:
    api_key: "sk-..."

agents:
  provider: anthropic
  model: claude-sonnet-4-6
```

Or use environment variables:

```bash
export ANTHROPIC_API_KEY="sk-..."
anna chat
```

## Architecture

```
                        anna
  +-----------+      +----------------+
  | CLI Chat  |----->|                |
  +-----------+      |     Pool       |   LLM Providers
                     | (sessions +   |<--> Anthropic / OpenAI
  +-----------+      |  Go runner)   |   HTTP API
  | Telegram  |----->|                |
  | LongPoll  |      +-------+--------+
  +-----------+              |
                     +-------v--------+
  +-----------+      |  Dispatcher   |
  |   Cron    |----->| (notify tool) |--> Telegram
  +-----------+      |               |--> (future backends)
                     +---------------+
```

```
main.go                             Entry point, CLI commands, service wiring
config.go                           Config types, YAML loading, env var overrides
models.go                           Model cache, discovery, CLI model commands
agent/pool.go                       Session management, runner lifecycle
agent/session.go                    Per-chat session state
agent/store/                        Session persistence (JSONL file store)
agent/runner/go/                    Go runner: native LLM provider calls
agent/runner/go/tool/               Built-in tools (read, bash, write, edit, truncate)
agent/runner/go/prompt.go           System prompt builder
channel/notifier.go                 Notification dispatcher (multi-backend)
channel/notify_tool.go              Agent notify tool
channel/telegram/                   Telegram bot + streaming + notification backend
channel/cli/                        Interactive terminal chat (Bubble Tea TUI)
cron/                               Scheduled jobs (gocron/v2)
memory/                             Persistent memory (facts + journal)
pkg/ai/providers/                   LLM provider implementations
pkg/ai/providers/anthropic/         Anthropic provider
pkg/ai/providers/openai/            OpenAI provider
pkg/ai/providers/openai-response/   OpenAI-compatible APIs (Responses API)
pkg/agent/core/                     Agent loop engine
```

## Documentation

| Document | Description |
|----------|-------------|
| [Deployment](docs/deployment.md) | Binary install, Docker, systemd, compose |
| [Configuration](docs/configuration.md) | Full config reference, env vars, defaults |
| [Architecture](docs/architecture.md) | System design, packages, providers, tools |
| [Telegram](docs/telegram.md) | Bot setup, streaming, groups, access control |
| [Models](docs/models.md) | Tiers, CLI commands, provider setup, caching |
| [Memory System](docs/memory-system.md) | Facts + journal, tool interface |
| [Cron System](docs/cron-system.md) | Scheduled tasks, job persistence |
| [Session Compaction](docs/session-compaction.md) | History compaction, token management |
| [Notification System](docs/notification-system.md) | Dispatcher, backends, agent tool |

## Development

Uses [mise](https://mise.jdx.dev/) for task automation:

```bash
mise run build          # Build binary -> bin/anna
mise run test           # Run tests with race detection
mise run lint           # go vet
mise run format         # gofmt + go mod tidy
mise run run:chat       # Build + run CLI chat
mise run run:stream     # Build + run streaming chat
mise run run:gateway    # Build + run gateway daemon
mise run clean          # Remove build artifacts
```

Or with plain Go:

```bash
go build -o anna .
go test -race ./...
```

## License

MIT
