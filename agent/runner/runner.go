package runner

import (
	"context"
	"encoding/json"
	"time"
)

// RPCCommand is sent to Pi's stdin as NDJSON.
type RPCCommand struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
}

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

// Event is the consumer-facing stream event. Channels read these from the
// stream returned by Pool.Chat().
type Event struct {
	Text string
	Err  error
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
		Type:                  "message_update",
		AssistantMessageEvent: inner,
	}
}
