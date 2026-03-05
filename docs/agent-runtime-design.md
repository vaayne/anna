# Agent Runtime Design

## Status

Draft — brainstorm output, not yet implemented.

## Problem

The current agent system is tightly coupled to a single runtime: spawning a
local Pi process and communicating via NDJSON over stdin/stdout. We want to
support multiple runtime backends (local process, pure Go function, Docker
container, remote REST) while keeping channels (CLI, Telegram) unchanged.

## Goals

1. Channels should not know or care which runner is behind a session.
2. Swapping the runner for an existing session must preserve conversation
   history — no data loss on runner change or crash.
3. The system should support crash recovery: respawn a runner and replay
   history transparently.
4. Pi RPC remains the canonical wire protocol. Other runners adapt to it.

## Architecture

```
 Channels (CLI, Telegram, ...)
     |
     |  sessionID + message
     v
 +------------------+
 |      Pool        |  owns sessions, owns history, manages lifecycle
 |                  |
 |  sessions:       |
 |    id -> Session |
 |      .Events     |  full Pi RPC event log (source of truth)
 |      .Runner     |  current runner (swappable)
 +--------+---------+
          |
          |  Chat(ctx, history, message)
          v
 +-----------------+
 |  Runner (i/f)   |  stateless — receives history each call
 +-----------------+
          |
    +-----+-------+--------+
    |     |       |        |
 Process GoFunc Docker   REST
 Runner  Runner Runner   Runner
```

## Naming

All types live in package `agent`. The package name provides domain context
(agent.Runner = "agent runner", agent.Pool = "agent pool"), so type names
describe the role, not the domain.

| Type | Role |
|------|------|
| `Runner` | Runs prompts against an AI backend. Stateless. |
| `Pool` | Manages sessions: maps session IDs to history + runner, handles lifecycle. |
| `NewRunnerFunc` | Constructor function that creates a Runner. |
| `Session` | A single conversation: history ([]RPCEvent) + assigned Runner. |
| `Event` | Consumer-facing stream event (text delta or error). |

## Core Types

### RPCEvent (Pi RPC event, stored verbatim)

Pool stores the full Pi RPC event stream per session. This is the canonical
representation — no lossy conversion to a simplified message type.

```go
// RPCEvent is the raw event from Pi's stdout NDJSON stream.
// Pool stores these verbatim as the session history.
type RPCEvent struct {
    Type                  string          `json:"type"`
    AssistantMessageEvent json.RawMessage `json:"assistantMessageEvent,omitempty"`
    ID                    string          `json:"id,omitempty"`
    Result                json.RawMessage `json:"result,omitempty"`
    Error                 string          `json:"error,omitempty"`
    Tool                  string          `json:"tool,omitempty"`
    Summary               string          `json:"summary,omitempty"`
}
```

Known event types from Pi RPC:

| Type             | Meaning                                    |
|------------------|--------------------------------------------|
| `message_update` | Streaming text delta from the assistant     |
| `agent_end`      | Agent finished processing the prompt        |
| `response`       | RPC request-response (matched by ID)        |
| `error`          | Error from the agent                        |

The `message_update` event contains an inner `assistantMessageEvent`:

```go
type assistantMessageEvent struct {
    Type  string `json:"type"`   // "text_delta"
    Delta string `json:"delta"`  // the text chunk
}
```

### RPCCommand (sent to Pi)

```go
type RPCCommand struct {
    ID      string `json:"id"`
    Type    string `json:"type"`              // "prompt"
    Message string `json:"message,omitempty"`
}
```

### Event (emitted to channels)

This is the consumer-facing type. Channels read these from the stream
returned by `Pool.Chat()`.

```go
type Event struct {
    Text string  // streamed text delta
    Err  error   // non-nil on error or stream end
}
```

### Session

```go
type Session struct {
    Events []RPCEvent  // full event log, append-only
    Runner Runner      // current runner, swappable
}
```

### Runner interface

```go
// Runner runs prompts against an AI backend.
// It is stateless — it receives full history each call and must
// reconstruct context from it.
type Runner interface {
    Chat(ctx context.Context, history []RPCEvent, message string) <-chan Event
}
```

Runners that hold resources (processes, connections) also implement
`io.Closer`. Pool calls `Close()` when evicting or swapping.

### NewRunnerFunc

```go
// NewRunnerFunc creates a new Runner instance.
// It does not receive a session ID — runners are stateless.
type NewRunnerFunc func(ctx context.Context) (Runner, error)
```

### Pool

```go
// Pool manages a set of sessions, each with its own history and runner.
// It is the only type channels interact with.
type Pool struct { ... }

func NewPool(factory NewRunnerFunc, opts ...PoolOption) *Pool

// Chat sends a message in a session and streams back events.
// Internally: gets/creates runner, passes history, collects events,
// appends to session log, streams to caller.
func (p *Pool) Chat(ctx context.Context, sessionID string, message string) <-chan Event

// Reset clears session history and closes the current runner.
func (p *Pool) Reset(sessionID string) error

// SetRunner swaps the runner for a session. History is preserved.
func (p *Pool) SetRunner(sessionID string, runner Runner) error

// Close shuts down all sessions and runners.
func (p *Pool) Close() error
```

## Pool.Chat Flow

```
1. Look up session by ID (create if new)
2. If session has no runner, create one via NewRunnerFunc
3. Call runner.Chat(ctx, session.Events, message)
4. For each Event from the runner:
   a. Convert to RPCEvent, append to session.Events
   b. Forward Event to the caller's channel
5. On completion or error, stop collecting
```

## Session History: Why Full RPCEvents

Storing the full RPCEvent stream (not just user/assistant text) gives us:

- **Lossless replay**: When swapping to a new runner or recovering from a
  crash, the new runner gets everything — tool calls, summaries, errors —
  not just the text.
- **Portable**: Save `[]RPCEvent` to disk as NDJSON. Load it back. Hand it
  to any runner.
- **Pi compatible**: Pi running with `--no-session` can accept a history of
  its own events natively. No translation layer needed for the process
  runner.
- **Debuggable**: The event log is a complete record of what happened.

## Runner Implementations

### ProcessRunner (current)

Spawns Pi as a local process with `--no-session`. On each `Chat()` call,
replays history events to Pi's stdin, then sends the new prompt. Reads
NDJSON from stdout.

Pi with `--no-session` means it keeps no session file — Pool owns the
history and replays it each time.

```go
type ProcessRunner struct {
    binary string
    model  string
}
```

### FuncRunner (future)

Pure Go implementation. No external process. Calls an LLM API directly
(e.g., OpenAI, Anthropic). Converts `[]RPCEvent` history to the API's
message format (extracting user/assistant text turns).

```go
type FuncRunner struct {
    handler func(ctx context.Context, history []RPCEvent, msg string) <-chan Event
}
```

### DockerRunner (future)

Runs Pi inside a Docker container. Same NDJSON protocol as ProcessRunner
but over `docker exec -i` instead of a direct process.

```go
type DockerRunner struct {
    image string
    model string
}
```

### RESTRunner (future)

Calls a remote HTTP endpoint. Translates RPCEvent history to the
endpoint's expected format. Reads SSE or chunked response back as Events.

```go
type RESTRunner struct {
    url string
}
```

## Configuration

```yaml
runner:
  type: process          # process | func | docker | rest
  process:
    binary: pi
    model: ""
  docker:
    image: ghcr.io/example/pi:latest
    model: ""
  rest:
    url: http://localhost:8080

sessions: .agents/workspace/sessions
idle_timeout: 10  # minutes

telegram:
  token: ""
```

## File Layout

```
agent/
    runner.go           # Runner interface, Event, RPCEvent, RPCCommand
    pool.go             # Pool implementation (session management, history, lifecycle)
    session.go          # Session type, history persistence (save/load NDJSON)
    process_runner.go   # ProcessRunner — local Pi process, --no-session
    func_runner.go      # FuncRunner — pure Go (future)
    docker_runner.go    # DockerRunner — Docker container (future)
    rest_runner.go      # RESTRunner — remote HTTP (future)
```

## Migration Path

1. Extract `Runner` interface and `Event`/`RPCEvent` types into
   `runner.go`.
2. Build `Pool` with session history management in `pool.go`.
3. Rename current `Agent` struct to `ProcessRunner`, implement the `Runner`
   interface. Switch to `--no-session` mode.
4. Update callers (main.go, channels) to use `Pool` instead of
   `SessionManager`.
5. Add new runners incrementally — each is an independent file.

## Open Questions

- **History replay cost**: Replaying the full event log on every `Chat()`
  call may be slow for long sessions. Consider a compaction strategy
  (e.g., summarize older turns) or let the runner decide how much history
  to use.
- **Tool state**: Tool call results are in the RPCEvent log. A FuncRunner
  that doesn't support tools will ignore them. Is that acceptable, or do
  we need a translation layer?
- **Persistence format**: NDJSON files per session (one RPCEvent per line)
  is simple and grep-friendly. SQLite is another option if we need
  querying. Start with NDJSON.
