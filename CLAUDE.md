## Project Overview

anna is a minimal Go CLI that acts as a local AI assistant. It spawns Pi (a coding agent) as a local process and communicates via JSON-RPC over stdin/stdout. Two interfaces: interactive CLI chat and Telegram bot (long polling).

## Architecture

```
main.go                         → Entry point, signal handling, wiring
config.go                       → Config types, YAML loading, env var overrides
agent/runner.go                 → Runner interface, Event, RPCEvent, RPCCommand types
agent/pool.go                   → Pool: session management, history, runner lifecycle
agent/session.go                → Session: event history + runner
agent/process_runner.go         → ProcessRunner: Pi process via NDJSON stdin/stdout
channel/telegram/telegram.go    → Telegram long polling, message splitting
channel/cli/cli.go              → Interactive terminal chat
channel/cli/chat.go             → Bubble Tea TUI chat model
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
Agent data: `.agents/pi` (set via `PI_CODING_AGENT_DIR` env)
Session data: `.agents/workspace/sessions`

Env var overrides:
- `ANNA_TELEGRAM_TOKEN` → telegram.token
- `ANNA_PI_BINARY` → runner.process.binary
- `ANNA_PI_MODEL` → runner.process.model

## Code Conventions

- Channel-based package structure: `main`, `agent`, `channel/cli`, `channel/telegram`
- Minimal dependencies (only `gopkg.in/yaml.v3`)
- stdlib `net/http` for Telegram API (no third-party bot libraries)
- `json.Decoder` for NDJSON reading (not `bufio.Scanner`)
- Conventional commits with emoji prefixes

## Testing

- Tests use `_test.go` files in each package
- Agent tests use mock processes (shell scripts or `echo` commands) instead of real Pi
- Config tests use temp directories
- Target: >80% coverage
- Run: `mise run test`
