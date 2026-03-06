package runtime

import (
	"context"
	"testing"

	"github.com/vaayne/anna/pkg/agent/core"
	agenttypes "github.com/vaayne/anna/pkg/agent/types"
	"github.com/vaayne/anna/pkg/ai/registry"
	"github.com/vaayne/anna/pkg/ai/stream"
	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

type fakeProvider struct{}

func (f fakeProvider) API() string { return "fake" }
func (f fakeProvider) Stream(model aitypes.Model, ctx aitypes.Context, opts aitypes.StreamOptions) (stream.AssistantEventStream, error) {
	s := stream.NewChannelEventStream(8)
	go func() {
		s.Emit(aitypes.EventTextDelta{Text: "ok"})
		s.Emit(aitypes.EventStop{Reason: aitypes.StopReasonStop})
		s.Finish(nil)
	}()
	return s, nil
}
func (f fakeProvider) StreamSimple(model aitypes.Model, ctx aitypes.Context, opts aitypes.SimpleStreamOptions) (stream.AssistantEventStream, error) {
	return f.Stream(model, ctx, opts.StreamOptions)
}

func TestAgentPromptAndWait(t *testing.T) {
	r := registry.New()
	r.Register(fakeProvider{})
	a := New(&core.Engine{Providers: r}, agenttypes.Config{Model: aitypes.Model{API: "fake", Name: "stub"}})
	if err := a.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if err := a.Prompt(context.Background(), "hello"); err != nil {
		t.Fatalf("unexpected prompt error: %v", err)
	}
	if err := a.Wait(); err != nil {
		t.Fatalf("unexpected wait error: %v", err)
	}
	if len(a.History()) != 2 {
		t.Fatalf("expected history length 2, got %d", len(a.History()))
	}
}
