package runner

import (
	"context"
	"encoding/json"
	"time"

	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

// RPCCommand is sent to Pi's stdin as NDJSON.
type RPCCommand struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
}

// RPCEvent type constants.
const (
	RPCEventUserMessage   = "user_message"
	RPCEventMessageUpdate = "message_update"
	RPCEventToolCall      = "tool_call"
	RPCEventToolResult    = "tool_result"
	RPCEventToolStart     = "tool_start"
	RPCEventToolEnd       = "tool_end"
	RPCEventAgentEnd      = "agent_end"
)

// RPCEvent is received from Pi's stdout as NDJSON.
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

// AssistantMessageEvent represents the inner event for text deltas.
type AssistantMessageEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
}

// ToolUseEvent describes a tool invocation in progress or completed.
type ToolUseEvent struct {
	Tool   string // tool name, e.g. "bash", "read"
	Status string // "running", "done", "error"
	Input  string // short summary of the tool input
	Detail string // error detail or result summary (for "error" status)
}

// Event is the consumer-facing stream event. Channels read these from the
// stream returned by Pool.Chat().
type Event struct {
	Text    string
	ToolUse *ToolUseEvent
	Store   *RPCEvent // if set, Pool appends to session history
	Err     error
}

// Runner runs prompts against an AI backend.
// It is stateless — it receives full history each call and must
// reconstruct context from it.
type Runner interface {
	Chat(ctx context.Context, history []RPCEvent, message string) <-chan Event
}

// NewRunnerFunc creates a new Runner instance.
type NewRunnerFunc func(ctx context.Context) (Runner, error)

// HandlerFunc is an adapter to allow the use of ordinary functions as Runners.
// If f is a function with the appropriate signature, HandlerFunc(f) is a Runner
// that calls f.
type HandlerFunc func(ctx context.Context, history []RPCEvent, message string) <-chan Event

// Chat calls f(ctx, history, message).
func (f HandlerFunc) Chat(ctx context.Context, history []RPCEvent, message string) <-chan Event {
	return f(ctx, history, message)
}

// Stateful is an optional interface for runners that maintain their own
// context in-process (e.g., a long-running subprocess). When a runner is
// Stateful, Pool will not kill it after compaction — the runner keeps its
// live context and the compacted history is only persisted to disk for
// crash recovery.
type Stateful interface {
	Stateful() bool
}

// Aliver is an optional interface for runners that can report liveness.
type Aliver interface {
	Alive() bool
}

// ActivityTracker is an optional interface for runners that track last activity.
type ActivityTracker interface {
	LastActivity() time.Time
}

// TextDeltaToRPCEvent converts a text delta string to an RPCEvent for storage.
func TextDeltaToRPCEvent(text string) RPCEvent {
	inner, _ := json.Marshal(AssistantMessageEvent{Type: "text_delta", Delta: text})
	return RPCEvent{
		Type:                  RPCEventMessageUpdate,
		AssistantMessageEvent: inner,
	}
}

// AssistantMessageToRPCEvent converts a complete assistant message to an RPCEvent.
func AssistantMessageToRPCEvent(text string) RPCEvent {
	return RPCEvent{
		Type:    RPCEventMessageUpdate,
		Summary: text,
	}
}

// ToolCallToRPCEvent converts a tool call to an RPCEvent for history storage.
func ToolCallToRPCEvent(call aitypes.ToolCall) RPCEvent {
	argsJSON, _ := json.Marshal(call.Arguments)
	return RPCEvent{
		Type:   RPCEventToolCall,
		ID:     call.ID,
		Tool:   call.Name,
		Result: argsJSON,
	}
}

// ToolResultToRPCEvent converts a tool result to an RPCEvent for history storage.
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
