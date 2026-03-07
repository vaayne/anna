package runner

import (
	"context"

	aitypes "github.com/vaayne/anna/ai/types"
)

// ToolFunc executes one tool invocation.
type ToolFunc func(ctx context.Context, call aitypes.ToolCall) (aitypes.TextContent, error)

// ToolSet maps tool names to handlers.
type ToolSet map[string]ToolFunc

// LoopConfig configures the agent loop behavior.
type LoopConfig struct {
	Model           aitypes.Model
	StreamOptions   aitypes.StreamOptions
	MaxTurns        int
	Tools           ToolSet
	ToolDefinitions []aitypes.ToolDefinition
	System          string
	Interrupt       <-chan struct{}
}
