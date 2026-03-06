package core

import (
	"context"
	"errors"

	agenttypes "github.com/vaayne/anna/pkg/agent/types"
	"github.com/vaayne/anna/pkg/ai/stream"
	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

// Engine coordinates model generation and tool execution.
type Engine struct {
	Providers stream.ProviderGetter
}

// Run executes one agent loop turn and optional tool pass.
func (e *Engine) Run(ctx context.Context, cfg agenttypes.Config, history []aitypes.Message, emit func(agenttypes.Event)) ([]aitypes.Message, error) {
	if e == nil || e.Providers == nil {
		return nil, errors.New("engine providers not configured")
	}
	if emit != nil {
		emit(agenttypes.AgentStarted{})
	}

	complete, err := stream.Complete(
		cfg.Model,
		aitypes.Context{Messages: history},
		aitypes.CompleteOptions{StreamOptions: cfg.StreamOptions},
		e.Providers,
	)
	if err != nil {
		if emit != nil {
			emit(agenttypes.AgentErrored{Err: err})
		}
		return history, err
	}

	history = append(history, complete)
	if emit != nil {
		emit(agenttypes.AssistantEmitted{Message: complete})
	}

	calls := extractToolCalls(complete)
	if len(calls) == 0 {
		if emit != nil {
			emit(agenttypes.AgentFinished{})
		}
		return history, nil
	}

	if cfg.Interrupt != nil {
		select {
		case <-cfg.Interrupt:
			if emit != nil {
				emit(agenttypes.AgentFinished{})
			}
			return history, nil
		default:
		}
	}

	results, err := ExecuteToolCalls(ctx, calls, cfg.Tools, ToolCallbacks{
		OnStart: func(call aitypes.ToolCall) {
			if emit != nil {
				emit(agenttypes.ToolStarted{ToolCall: call})
			}
		},
		OnFinish: func(result aitypes.ToolResultMessage) {
			if emit != nil {
				emit(agenttypes.ToolFinished{Result: result})
			}
		},
	})
	if err != nil {
		if emit != nil {
			emit(agenttypes.AgentErrored{Err: err})
		}
		return history, err
	}

	for _, result := range results {
		history = append(history, result)
	}

	if emit != nil {
		emit(agenttypes.AgentFinished{})
	}

	return history, nil
}

func extractToolCalls(msg aitypes.AssistantMessage) []aitypes.ToolCall {
	calls := make([]aitypes.ToolCall, 0, 2)
	for _, block := range msg.Content {
		call, ok := block.(aitypes.ToolCall)
		if ok {
			calls = append(calls, call)
		}
	}
	return calls
}
