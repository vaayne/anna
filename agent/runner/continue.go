package runner

import (
	"context"
	"errors"

	aitypes "github.com/vaayne/anna/ai/types"
)

// Continue validates that the transcript tail is a user or tool-result message
// and resumes the agent loop from the existing history.
func (e *Engine) Continue(ctx context.Context, cfg LoopConfig, history []aitypes.Message, emit func(loopEvent)) ([]aitypes.Message, error) {
	if len(history) == 0 {
		return nil, errors.New("cannot continue empty history")
	}

	switch history[len(history)-1].(type) {
	case aitypes.UserMessage, aitypes.ToolResultMessage:
		return e.Run(ctx, cfg, history, emit)
	default:
		return nil, errors.New("invalid transcript tail for continue")
	}
}
