package core

import (
	"context"
	"errors"
	"fmt"

	agenttypes "github.com/vaayne/anna/pkg/agent/types"
	"github.com/vaayne/anna/pkg/ai/stream"
	"github.com/vaayne/anna/pkg/ai/transform"
	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

// Engine coordinates model generation and tool execution.
type Engine struct {
	Providers stream.ProviderGetter
}

// Run executes the agent loop: repeatedly generating assistant responses
// and executing tool calls until the model stops calling tools,
// the turn limit is reached, or an interrupt/error occurs.
func (e *Engine) Run(ctx context.Context, cfg agenttypes.Config, history []aitypes.Message, emit func(agenttypes.Event)) ([]aitypes.Message, error) {
	if e == nil || e.Providers == nil {
		return nil, errors.New("engine providers not configured")
	}
	if emit != nil {
		emit(agenttypes.AgentStarted{})
	}

	history, err := e.runLoop(ctx, cfg, history, emit)
	if err != nil {
		if emit != nil {
			emit(agenttypes.AgentErrored{Err: err})
		}
		return history, err
	}

	if emit != nil {
		emit(agenttypes.AgentFinished{})
	}
	return history, nil
}

func (e *Engine) runLoop(ctx context.Context, cfg agenttypes.Config, history []aitypes.Message, emit func(agenttypes.Event)) ([]aitypes.Message, error) {
	maxTurns := cfg.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 128
	}

	for turn := 1; turn <= maxTurns; turn++ {
		if emit != nil {
			emit(agenttypes.TurnStarted{Turn: turn})
		}

		// Normalize transcript before each model call.
		normalized := transform.Messages(history)

		complete, err := streamAssistant(normalized, cfg, e.Providers, emit)
		if err != nil {
			return history, err
		}

		history = append(history, complete)

		// Check stop reason for terminal conditions.
		if complete.StopReason == aitypes.StopReasonError || complete.StopReason == aitypes.StopReasonAborted {
			if emit != nil {
				emit(agenttypes.TurnFinished{Turn: turn})
			}
			return history, nil
		}

		calls := extractToolCalls(complete)
		if len(calls) == 0 {
			if emit != nil {
				emit(agenttypes.TurnFinished{Turn: turn})
			}
			return history, nil
		}

		// Check interrupt before executing tools.
		if cfg.Interrupt != nil {
			select {
			case <-cfg.Interrupt:
				if emit != nil {
					emit(agenttypes.TurnFinished{Turn: turn})
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
			return history, err
		}

		for _, result := range results {
			history = append(history, result)
		}

		if emit != nil {
			emit(agenttypes.TurnFinished{Turn: turn})
		}
	}

	return history, fmt.Errorf("agent loop exceeded maximum turns (%d)", maxTurns)
}

// streamAssistant opens a provider stream, emits granular assistant events,
// and assembles the final AssistantMessage.
func streamAssistant(messages []aitypes.Message, cfg agenttypes.Config, providers stream.ProviderGetter, emit func(agenttypes.Event)) (aitypes.AssistantMessage, error) {
	eventStream, err := stream.Stream(
		cfg.Model,
		aitypes.Context{Messages: messages},
		cfg.StreamOptions,
		providers,
	)
	if err != nil {
		return aitypes.AssistantMessage{}, err
	}

	msg := aitypes.AssistantMessage{Content: make([]aitypes.ContentBlock, 0, 4)}
	var text string
	var thinking string
	toolCalls := map[string]aitypes.ToolCall{}
	started := false

	for event := range eventStream.Events() {
		switch e := event.(type) {
		case aitypes.EventTextDelta:
			text += e.Text
		case aitypes.EventThinkingDelta:
			thinking += e.Thinking
		case aitypes.EventToolCallDelta:
			call := toolCalls[e.ID]
			call.ID = e.ID
			if e.Name != "" {
				call.Name = e.Name
			}
			if call.Arguments == nil {
				call.Arguments = map[string]any{}
			}
			if e.Arguments != "" {
				call.Arguments["raw"] = e.Arguments
			}
			toolCalls[e.ID] = call
		case aitypes.EventUsage:
			msg.Usage = e.Usage
		case aitypes.EventStop:
			msg.StopReason = e.Reason
		case aitypes.EventError:
			if e.Err != nil {
				msg.ErrorMessage = e.Err.Error()
				msg.StopReason = aitypes.StopReasonError
			}
		}

		// Emit AssistantStarted on first event.
		if !started {
			started = true
			if emit != nil {
				emit(agenttypes.AssistantStarted{Message: msg})
			}
		}

		// Emit every provider event as a delta.
		if emit != nil {
			// Build current partial for the delta snapshot.
			partial := buildPartial(msg, text, thinking, toolCalls)
			emit(agenttypes.AssistantDelta{Event: event, Message: partial})
		}
	}

	if waitErr := eventStream.Wait(); waitErr != nil {
		return msg, waitErr
	}

	// Assemble final message.
	if text != "" {
		msg.Content = append(msg.Content, aitypes.TextContent{Text: text})
	}
	if thinking != "" {
		msg.Content = append(msg.Content, aitypes.ThinkingContent{Thinking: thinking})
	}
	for _, call := range toolCalls {
		msg.Content = append(msg.Content, call)
	}

	if emit != nil {
		emit(agenttypes.AssistantFinished{Message: msg})
	}

	return msg, nil
}

// buildPartial constructs a snapshot of the in-progress assistant message.
func buildPartial(base aitypes.AssistantMessage, text, thinking string, toolCalls map[string]aitypes.ToolCall) aitypes.AssistantMessage {
	partial := base
	partial.Content = make([]aitypes.ContentBlock, 0, 4)
	if text != "" {
		partial.Content = append(partial.Content, aitypes.TextContent{Text: text})
	}
	if thinking != "" {
		partial.Content = append(partial.Content, aitypes.ThinkingContent{Thinking: thinking})
	}
	for _, call := range toolCalls {
		partial.Content = append(partial.Content, call)
	}
	return partial
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
