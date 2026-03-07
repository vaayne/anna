package stream_test

import (
	"testing"

	"github.com/vaayne/anna/ai/registry"
	"github.com/vaayne/anna/ai/stream"
	"github.com/vaayne/anna/ai/types"
)

type fakeProvider struct{}

func (f fakeProvider) API() string { return "fake" }

func (f fakeProvider) Stream(model types.Model, ctx types.Context, opts types.StreamOptions) (stream.AssistantEventStream, error) {
	out := stream.NewChannelEventStream(8)
	go func() {
		out.Emit(types.EventStart{})
		out.Emit(types.EventTextDelta{Text: "hello"})
		out.Emit(types.EventStop{Reason: types.StopReasonStop})
		out.Emit(types.EventUsage{Usage: types.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}})
		out.Finish(nil)
	}()
	return out, nil
}

func (f fakeProvider) StreamSimple(model types.Model, ctx types.Context, opts types.SimpleStreamOptions) (stream.AssistantEventStream, error) {
	return f.Stream(model, ctx, opts.StreamOptions)
}

func TestCompleteWithRegistry(t *testing.T) {
	r := registry.New()
	r.Register(fakeProvider{})

	message, err := stream.Complete(types.Model{API: "fake", Name: "stub"}, types.Context{}, types.CompleteOptions{}, r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(message.Content) != 1 {
		t.Fatalf("expected one content block, got %d", len(message.Content))
	}
	text, ok := message.Content[0].(types.TextContent)
	if !ok || text.Text != "hello" {
		t.Fatalf("unexpected content: %#v", message.Content[0])
	}
	if message.Usage.TotalTokens != 2 {
		t.Fatalf("expected usage total 2, got %d", message.Usage.TotalTokens)
	}
}
