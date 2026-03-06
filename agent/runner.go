package agent

import (
	"context"
	"encoding/json"
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

// assistantMessageEvent represents the inner event for text deltas.
type assistantMessageEvent struct {
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
