package core

import (
	"context"
	"errors"

	agenttypes "github.com/vaayne/anna/pkg/agent/types"
	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

// Continue validates transcript tail and performs one additional loop.
func (e *Engine) Continue(ctx context.Context, cfg agenttypes.Config, history []aitypes.Message, emit func(agenttypes.Event)) ([]aitypes.Message, error) {
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
