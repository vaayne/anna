# Agent Runtime Design

## Status

Implemented — core architecture (runner package, Pool, pi.Runner, HandlerFunc) is live.

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
 |  (agent pkg)     |
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
 | (runner pkg)    |
 +-----------------+
          |
    +-----+----------+--------+
    |     |          |        |
   pi  Handler-  Docker   REST
 Runner  Func    Runner   Runner
```

## Package Layout

```
agent/
    runner/
        runner.go           # Runner interface, Event, RPCEvent, RPCCommand,
                            # NewRunnerFunc, HandlerFunc, optional interfaces
                            # (Aliver, ActivityTracker), TextDeltaToRPCEvent
        pi/
            runner.go       # pi.Runner — local Pi process, --no-session
            runner_test.go
    pool.go                 # Pool (session management, history, lifecycle)
    session.go              # Session type (event history + runner reference)
    pool_test.go
```

## Naming

Types are split across two packages. The `runner` package holds the interface
and wire types. The `agent` package holds session management (Pool, Session).
Each runner implementation lives in its own sub-package under `runner/`.

| Type | Package | Role |
|------|---------|------|
| `Runner` | `runner` | Runs prompts against an AI backend. Stateless. |
| `Event` | `runner` | Consumer-facing stream event (text delta or error). |
| `RPCEvent` | `runner` | Pi RPC event, stored verbatim as session history. |
| `RPCCommand` | `runner` | Command sent to Pi's stdin. |
| `NewRunnerFunc` | `runner` | Constructor function that creates a Runner. |
| `HandlerFunc` | `runner` | Adapter: wraps a Go function as a Runner (like `http.HandlerFunc`). |
| `Aliver` | `runner` | Optional interface: `Alive() bool`. |
| `ActivityTracker` | `runner` | Optional interface: `LastActivity() time.Time`. |
| `Pool` | `agent` | Manages sessions: maps session IDs to history + runner, handles lifecycle. |
| `Session` | `agent` | A single conversation: history ([]RPCEvent) + assigned Runner. |
| `Runner` | `pi` | Pi process runner — spawns Pi with `--no-session`, NDJSON over stdin/stdout. |

## Core Types

### RPCEvent (Pi RPC event, stored verbatim)

Pool stores the full Pi RPC event stream per session. This is the canonical
representation — no lossy conversion to a simplified message type.

```go
// In package runner

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

The `message_update` event contains an inner `AssistantMessageEvent`:

```go
type AssistantMessageEvent struct {
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
// In package agent

type Session struct {
    Events []runner.RPCEvent  // full event log, append-only
    Runner runner.Runner      // current runner, swappable
}
```

### Runner interface

```go
// In package runner

type Runner interface {
    Chat(ctx context.Context, history []RPCEvent, message string) <-chan Event
}
```

Runners that hold resources (processes, connections) also implement
`io.Closer`. Pool calls `Close()` when evicting or swapping.

### HandlerFunc

```go
// HandlerFunc is an adapter to allow ordinary functions as Runners.
// Same pattern as http.HandlerFunc → http.Handler.
type HandlerFunc func(ctx context.Context, history []RPCEvent, message string) <-chan Event

func (f HandlerFunc) Chat(ctx context.Context, history []RPCEvent, message string) <-chan Event {
    return f(ctx, history, message)
}
```

### Optional Interfaces

Pool uses type assertions to check for these, keeping it decoupled from
any specific runner implementation.

```go
// Aliver is an optional interface for runners that can report liveness.
type Aliver interface {
    Alive() bool
}

// ActivityTracker is an optional interface for runners that track last activity.
type ActivityTracker interface {
    LastActivity() time.Time
}
```

### NewRunnerFunc

```go
type NewRunnerFunc func(ctx context.Context) (Runner, error)
```

### Pool

```go
// In package agent

type Pool struct { ... }

func NewPool(factory runner.NewRunnerFunc, opts ...PoolOption) *Pool

func (p *Pool) Chat(ctx context.Context, sessionID string, message string) <-chan runner.Event

func (p *Pool) Reset(sessionID string) error

func (p *Pool) Close() error
```

## Pool.Chat Flow

```
1. Look up session by ID (create if new)
2. If session has no runner, create one via NewRunnerFunc
3. If runner implements Aliver and is dead, close and recreate
4. Call runner.Chat(ctx, session.Events, message)
5. For each Event from the runner:
   a. Convert to RPCEvent, append to session.Events
   b. Forward Event to the caller's channel
6. On completion or error, stop collecting
```

## Pool.reap Flow

```
For each session with a non-nil runner:
1. If runner implements Aliver and !Alive() → nil out runner
2. If runner implements ActivityTracker and idle > timeout → Close + nil out
3. Runners without these interfaces are left alone
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

### pi.Runner (current)

Spawns Pi as a local process with `--no-session`. On each `Chat()` call,
replays history events to Pi's stdin, then sends the new prompt. Reads
NDJSON from stdout. Implements `Runner`, `Aliver`, `ActivityTracker`,
and `io.Closer`.

```go
// In package pi (agent/runner/pi)

type Runner struct {
    binary string
    model  string
    // ... process management fields
}

func New(ctx context.Context, binary, model string) (*Runner, error)
```

### HandlerFunc (current)

Adapter in the `runner` package. Wraps any Go function as a Runner.
Use for inline implementations, testing, or direct LLM API calls.

```go
factory := func(ctx context.Context) (runner.Runner, error) {
    return runner.HandlerFunc(func(ctx context.Context, history []runner.RPCEvent, msg string) <-chan runner.Event {
        // call OpenAI, Anthropic, etc.
    }), nil
}
```

### DockerRunner (future)

Runs Pi inside a Docker container. Same NDJSON protocol as pi.Runner
but over `docker exec -i` instead of a direct process. Would live in
`agent/runner/docker/`.

### RESTRunner (future)

Calls a remote HTTP endpoint. Translates RPCEvent history to the
endpoint's expected format. Reads SSE or chunked response back as Events.
Would live in `agent/runner/rest/`.

## Configuration

```yaml
runner:
  type: process          # process | docker | rest
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

## Open Questions

- **History replay cost**: Replaying the full event log on every `Chat()`
  call may be slow for long sessions. Consider a compaction strategy
  (e.g., summarize older turns) or let the runner decide how much history
  to use.
- **Tool state**: Tool call results are in the RPCEvent log. A HandlerFunc
  that doesn't support tools will ignore them. Is that acceptable, or do
  we need a translation layer?
- **Persistence format**: NDJSON files per session (one RPCEvent per line)
  is simple and grep-friendly. SQLite is another option if we need
  querying. Start with NDJSON.
