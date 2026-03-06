package types

import (
	"context"

	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

// ToolFunc executes one tool invocation.
type ToolFunc func(ctx context.Context, call aitypes.ToolCall) (aitypes.ToolResultContent, error)

// ToolSet maps tool names to handlers.
type ToolSet map[string]ToolFunc

// Config configures the agent loop behavior.
type Config struct {
	Model         aitypes.Model
	StreamOptions aitypes.StreamOptions
	MaxTurns      int
	Tools         ToolSet
	Interrupt     <-chan struct{}
}
