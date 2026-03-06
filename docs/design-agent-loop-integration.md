# Agent Loop Integration Design

## Status

Proposed. The `pkg/agent/core.Engine` agent loop is built and tested but not wired into the Go runner.

## Problem

Anna has two parallel agent loop implementations:

1. **`pkg/agent/core.Engine`** -- real multi-turn loop with streaming events, max turns, interrupt, transcript normalization. Not connected to anything.
2. **`agent/runner/go/runner.go`** -- inline single-purpose loop (`streamOnce` + tool execution). Connected to CLI/Telegram via Pool.

These should be unified: the Go runner should use Engine internally.

Additionally, the session history (`[]RPCEvent`) only stores user messages and text deltas. Tool calls and tool results are discarded by Pool, making history incomplete and breaking stateless replay.

## Architecture

```
channel/cli/  --------+
channel/telegram/ -----+
                       v
                agent/pool.go              Session{Events []RPCEvent, Runner}
                       |
                runner.Runner              Chat(ctx, []RPCEvent, msg) <-chan runner.Event
                       |
                +------+------+
          runner/pi/      runner/go/
          (subprocess)    (uses Engine)
                               |
                         +-----+-----+
                   convertHistory()  convertEvent()
                   RPCEvent->Message agenttypes.Event->runner.Event
                               |
                     pkg/agent/core.Engine
                               |
                     pkg/ai/stream (providers)
```

### Layer responsibilities

| Layer | Responsibility | Types |
|---|---|---|
| **Channels** (cli, telegram) | UI rendering, user input | Consume `runner.Event` |
| **Pool** | Session lifecycle, history storage | `Session`, `[]RPCEvent` |
| **Runner interface** | Abstraction over agent runtimes | `runner.Runner`, `runner.Event`, `runner.RPCEvent` |
| **Pi runner** | Spawns Pi subprocess, NDJSON IPC | Unchanged |
| **Go runner** | Adapts Engine to Runner interface | Converts RPCEvent <-> aitypes.Message, bridges events |
| **Engine** | Multi-turn agent loop | `agenttypes.Event`, `aitypes.Message` |
| **Providers** | LLM streaming | `aitypes.AssistantEvent` |

## Changes

### 1. Expand RPCEvent with tool event types

**File:** `agent/runner/runner.go`

RPCEvent currently supports these types:

- `user_message` -- user input (Summary field)
- `message_update` -- text delta (AssistantMessageEvent field)

Add two new types for complete history:

- `tool_call` -- assistant requested a tool invocation
- `tool_result` -- tool execution output

```go
// RPCEvent type constants.
const (
    RPCEventUserMessage   = "user_message"
    RPCEventMessageUpdate = "message_update"
    RPCEventToolCall      = "tool_call"
    RPCEventToolResult    = "tool_result"
    RPCEventToolStart     = "tool_start"   // existing, ephemeral (Pi compat)
    RPCEventToolEnd       = "tool_end"     // existing, ephemeral (Pi compat)
    RPCEventAgentEnd      = "agent_end"    // existing, ephemeral (Pi compat)
)
```

Add conversion helpers:

```go
func ToolCallToRPCEvent(call aitypes.ToolCall) RPCEvent {
    argsJSON, _ := json.Marshal(call.Arguments)
    return RPCEvent{
        Type:   RPCEventToolCall,
        ID:     call.ID,
        Tool:   call.Name,
        Result: argsJSON,
    }
}

func ToolResultToRPCEvent(result aitypes.ToolResultMessage) RPCEvent {
    var text string
    for _, block := range result.Content {
        if tc, ok := block.(aitypes.TextContent); ok {
            text += tc.Text
        }
    }
    contentJSON, _ := json.Marshal(text)
    evt := RPCEvent{
        Type:   RPCEventToolResult,
        ID:     result.ToolCallID,
        Tool:   result.ToolName,
        Result: contentJSON,
    }
    if result.IsError {
        evt.Error = text
    }
    return evt
}
```

### 2. Pool stores tool events in history

**File:** `agent/pool.go`

Current behavior skips tool events:

```go
// BEFORE: tool events are ephemeral
if evt.ToolUse != nil {
    out <- evt
    continue  // not stored
}
```

New behavior stores them. However, `runner.Event` only has `ToolUse *ToolUseEvent` which is a display-oriented type (tool name + status string), not a structured type with IDs and arguments. The actual RPCEvent storage happens in the runner itself -- the runner emits text deltas (stored by Pool) and tool use events (display only).

**Design decision:** The Go runner is responsible for emitting RPCEvents for tool calls/results back through a new channel or callback. Two options:

**Option A (simple):** Go runner stores its own history internally by converting Engine output to RPCEvents after each `Chat()` call. Pool only needs the text deltas for display. On next `Chat()` call, the runner receives its own stored RPCEvents back.

**Option B (Pool stores everything):** Add an `RPCEvent` field to `runner.Event` so runners can emit storage events that Pool persists. This keeps Pool as the single history owner.

**Recommended: Option A.** It's simpler and matches the existing pattern where Pi runner also manages its own internal state. The RPCEvent history passed to `Chat()` serves as the reconstruction source, and each runner is responsible for emitting events that can reconstruct its state.

However, for Option A to work, the Go runner must emit tool_call and tool_result RPCEvents through the existing `runner.Event` channel so Pool can store them. This requires a small extension:

```go
// runner.Event gets one new field:
type Event struct {
    Text    string
    ToolUse *ToolUseEvent
    Store   *RPCEvent      // if set, Pool appends to session history
    Err     error
}
```

Pool logic becomes:

```go
for evt := range stream {
    if evt.Store != nil {
        sess.Events = append(sess.Events, *evt.Store)
    }
    // existing text/tooluse/error handling unchanged
    ...
}
```

This way runners can emit storage events without Pool needing to understand the semantics.

### 3. Expand convertHistory for tool events

**File:** `agent/runner/go/runner.go`

Current `convertHistory()` only handles `user_message` and `message_update`. Expand to handle `tool_call` and `tool_result`:

```go
func convertHistory(events []runner.RPCEvent) []aitypes.Message {
    var messages []aitypes.Message
    var textBuf string
    var pendingCalls []aitypes.ToolCall

    flush := func() {
        if textBuf != "" {
            messages = append(messages, aitypes.AssistantMessage{
                Content: []aitypes.ContentBlock{aitypes.TextContent{Text: textBuf}},
            })
            textBuf = ""
        }
    }

    flushToolCalls := func() {
        if len(pendingCalls) > 0 {
            blocks := make([]aitypes.ContentBlock, len(pendingCalls))
            for i, c := range pendingCalls {
                blocks[i] = c
            }
            // If there's buffered text, prepend it
            if textBuf != "" {
                blocks = append([]aitypes.ContentBlock{
                    aitypes.TextContent{Text: textBuf},
                }, blocks...)
                textBuf = ""
            }
            messages = append(messages, aitypes.AssistantMessage{Content: blocks})
            pendingCalls = nil
        }
    }

    for _, evt := range events {
        switch evt.Type {
        case "user_message":
            flushToolCalls()
            flush()
            messages = append(messages, aitypes.UserMessage{Content: evt.Summary})

        case "message_update":
            if len(evt.AssistantMessageEvent) > 0 {
                var ame runner.AssistantMessageEvent
                if json.Unmarshal(evt.AssistantMessageEvent, &ame) == nil && ame.Type == "text_delta" {
                    textBuf += ame.Delta
                }
            }

        case "tool_call":
            var args map[string]any
            _ = json.Unmarshal(evt.Result, &args)
            pendingCalls = append(pendingCalls, aitypes.ToolCall{
                ID:        evt.ID,
                Name:      evt.Tool,
                Arguments: args,
            })

        case "tool_result":
            // Flush any pending tool calls before their results
            flushToolCalls()
            var content string
            _ = json.Unmarshal(evt.Result, &content)
            messages = append(messages, aitypes.ToolResultMessage{
                ToolCallID: evt.ID,
                ToolName:   evt.Tool,
                Content:    []aitypes.ContentBlock{aitypes.TextContent{Text: content}},
                IsError:    evt.Error != "",
            })
        }
    }

    flushToolCalls()
    flush()
    return messages
}
```

### 4. Go runner uses Engine internally

**File:** `agent/runner/go/runner.go`

Replace the inline loop (`Chat` method + `streamOnce` + `toolCallAccumulator`) with Engine:

```go
func (r *Runner) Chat(ctx context.Context, history []runner.RPCEvent, message string) <-chan runner.Event {
    out := make(chan runner.Event, 100)

    r.mu.Lock()
    r.lastActivity = time.Now()
    r.mu.Unlock()

    go func() {
        defer close(out)

        messages := convertHistory(history)
        messages = append(messages, aitypes.UserMessage{Content: message})

        cfg := agenttypes.Config{
            Model:         r.model,
            StreamOptions: aitypes.StreamOptions{APIKey: r.apiKey},
            MaxTurns:      maxToolIterations,
            Tools:         r.buildToolSet(),
        }

        r.engine.Run(ctx, cfg, messages, func(e agenttypes.Event) {
            converted := convertEvent(e)
            for _, evt := range converted {
                out <- evt
            }
        })
    }()

    return out
}
```

### 5. Bridge agenttypes.Event to runner.Event

**File:** `agent/runner/go/runner.go`

```go
func convertEvent(e agenttypes.Event) []runner.Event {
    switch e := e.(type) {
    case agenttypes.AssistantDelta:
        if d, ok := e.Event.(aitypes.EventTextDelta); ok && d.Text != "" {
            return []runner.Event{{Text: d.Text}}
        }

    case agenttypes.AssistantFinished:
        // Emit Store events for tool calls in the final message
        var events []runner.Event
        for _, block := range e.Message.Content {
            if call, ok := block.(aitypes.ToolCall); ok {
                rpc := runner.ToolCallToRPCEvent(call)
                events = append(events, runner.Event{Store: &rpc})
            }
        }
        return events

    case agenttypes.ToolStarted:
        return []runner.Event{{ToolUse: &runner.ToolUseEvent{
            Tool:   e.ToolCall.Name,
            Status: "running",
            Input:  summarizeToolInput(e.ToolCall.Name, e.ToolCall.Arguments),
        }}}

    case agenttypes.ToolFinished:
        status := "done"
        detail := ""
        if e.Result.IsError {
            status = "error"
            for _, block := range e.Result.Content {
                if tc, ok := block.(aitypes.TextContent); ok {
                    detail = tc.Text
                }
            }
        }
        rpc := runner.ToolResultToRPCEvent(e.Result)
        return []runner.Event{
            {ToolUse: &runner.ToolUseEvent{
                Tool:   e.Result.ToolName,
                Status: status,
                Input:  summarizeToolInput(e.Result.ToolName, nil),
                Detail: detail,
            }},
            {Store: &rpc},
        }

    case agenttypes.AgentErrored:
        return []runner.Event{{Err: e.Err}}
    }

    return nil
}
```

### 6. Adapt tool.Registry to agenttypes.ToolSet

**File:** `agent/runner/go/runner.go`

```go
func (r *Runner) buildToolSet() agenttypes.ToolSet {
    set := agenttypes.ToolSet{}
    for _, def := range r.tools.Definitions() {
        name := def.Name
        set[name] = func(ctx context.Context, call aitypes.ToolCall) (aitypes.TextContent, error) {
            result, err := r.tools.Execute(ctx, name, call.Arguments)
            return aitypes.TextContent{Text: result}, err
        }
    }
    return set
}
```

### 7. Engine needs tool definitions in Context

**Current gap:** `pkg/agent/core.Engine` passes `aitypes.Context{Messages: normalized}` to `stream.Stream()`, but does not include `Tools` (tool definitions). The LLM needs tool definitions to know what tools are available.

**Fix:** Add `ToolDefinitions []aitypes.ToolDefinition` to `agenttypes.Config` and pass them through to `aitypes.Context.Tools` in `streamAssistant()`.

```go
// In agenttypes.Config, add:
type Config struct {
    Model           aitypes.Model
    StreamOptions   aitypes.StreamOptions
    MaxTurns        int
    Tools           ToolSet
    ToolDefinitions []aitypes.ToolDefinition  // NEW: sent to LLM
    Interrupt       <-chan struct{}
}
```

In `streamAssistant()`:

```go
eventStream, err := stream.Stream(
    cfg.Model,
    aitypes.Context{Messages: messages, Tools: cfg.ToolDefinitions},
    cfg.StreamOptions,
    providers,
)
```

## What to delete

| File | Reason |
|---|---|
| `agent/runner/go/runner.go`: `streamOnce()` | Replaced by Engine |
| `agent/runner/go/runner.go`: `toolCallAccumulator` | Replaced by Engine's stream assembly |
| `pkg/agent/runtime/agent.go` | Unused stateful wrapper; Pool fills this role |
| `pkg/agent/runtime/agent_test.go` | Tests for deleted code |

## What stays unchanged

| File | Reason |
|---|---|
| `runner.Runner` interface | Correct abstraction |
| `runner/pi/runner.go` | Pi runner is independent |
| `channel/cli/` | Consumes runner.Event, no changes needed |
| `channel/telegram/` | Same |
| `pkg/agent/core/loop.go` | Engine, already built |
| `pkg/agent/core/tool_execution.go` | Used by Engine |
| `pkg/ai/` | Provider layer, unchanged |

## Implementation order

1. Add RPCEvent constants and conversion helpers (`runner/runner.go`)
2. Add `Store *RPCEvent` field to `runner.Event` (`runner/runner.go`)
3. Pool stores `Store` events (`agent/pool.go`)
4. Add `ToolDefinitions` to `agenttypes.Config`, pass to Context (`pkg/agent/types/types.go`, `pkg/agent/core/loop.go`)
5. Expand `convertHistory()` for tool events (`runner/go/runner.go`)
6. Add `convertEvent()` bridge (`runner/go/runner.go`)
7. Add `buildToolSet()` adapter (`runner/go/runner.go`)
8. Replace Go runner `Chat()` with Engine-backed implementation (`runner/go/runner.go`)
9. Delete `streamOnce`, `toolCallAccumulator` (`runner/go/runner.go`)
10. Delete `pkg/agent/runtime/` (`pkg/agent/runtime/agent.go`, `pkg/agent/runtime/agent_test.go`)
11. Update tests
