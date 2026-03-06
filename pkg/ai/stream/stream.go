package stream

import (
	"context"
	"errors"

	"github.com/vaayne/anna/pkg/ai/types"
)

// Provider defines the provider adapter contract.
type Provider interface {
	API() string
	Stream(model types.Model, ctx types.Context, opts types.StreamOptions) (AssistantEventStream, error)
	StreamSimple(model types.Model, ctx types.Context, opts types.SimpleStreamOptions) (AssistantEventStream, error)
}

// ModelLister is an optional interface providers can implement to list available models.
type ModelLister interface {
	ListModels(ctx context.Context) ([]types.Model, error)
}

// ProviderGetter provides provider lookup.
type ProviderGetter interface {
	Get(api string) (Provider, bool)
}

// ErrProviderNotFound indicates missing provider registration.
var ErrProviderNotFound = errors.New("provider not found")

// Stream dispatches request to the registered API provider.
func Stream(model types.Model, ctx types.Context, opts types.StreamOptions, providers ProviderGetter) (AssistantEventStream, error) {
	provider, ok := providers.Get(model.API)
	if !ok {
		return nil, ErrProviderNotFound
	}
	return provider.Stream(model, ctx, opts)
}

// StreamSimple dispatches simplified streaming to provider.
func StreamSimple(model types.Model, ctx types.Context, opts types.SimpleStreamOptions, providers ProviderGetter) (AssistantEventStream, error) {
	provider, ok := providers.Get(model.API)
	if !ok {
		return nil, ErrProviderNotFound
	}
	return provider.StreamSimple(model, ctx, opts)
}

// Complete consumes a full stream and assembles an assistant message.
func Complete(model types.Model, ctx types.Context, opts types.CompleteOptions, providers ProviderGetter) (types.AssistantMessage, error) {
	eventStream, err := Stream(model, ctx, opts.StreamOptions, providers)
	if err != nil {
		return types.AssistantMessage{}, err
	}

	msg := types.AssistantMessage{Content: make([]types.ContentBlock, 0, 4)}
	var text string
	var thinking string
	toolCalls := map[string]types.ToolCall{}

	for event := range eventStream.Events() {
		switch e := event.(type) {
		case types.EventTextDelta:
			text += e.Text
		case types.EventThinkingDelta:
			thinking += e.Thinking
		case types.EventToolCallDelta:
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
		case types.EventUsage:
			msg.Usage = e.Usage
		case types.EventStop:
			msg.StopReason = e.Reason
		case types.EventError:
			if e.Err != nil {
				msg.ErrorMessage = e.Err.Error()
				msg.StopReason = types.StopReasonError
			}
		}
	}

	if waitErr := eventStream.Wait(); waitErr != nil {
		return msg, waitErr
	}

	if text != "" {
		msg.Content = append(msg.Content, types.TextContent{Text: text})
	}
	if thinking != "" {
		msg.Content = append(msg.Content, types.ThinkingContent{Thinking: thinking})
	}
	for _, call := range toolCalls {
		msg.Content = append(msg.Content, call)
	}

	return msg, nil
}
