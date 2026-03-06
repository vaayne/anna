package core

import (
	"context"
	"testing"

	agenttypes "github.com/vaayne/anna/pkg/agent/types"
	"github.com/vaayne/anna/pkg/ai/registry"
	"github.com/vaayne/anna/pkg/ai/stream"
	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

type fakeProvider struct{}

func (f fakeProvider) API() string { return "fake" }

func (f fakeProvider) Stream(model aitypes.Model, ctx aitypes.Context, opts aitypes.StreamOptions) (stream.AssistantEventStream, error) {
	out := stream.NewChannelEventStream(8)
	go func() {
		out.Emit(aitypes.EventTextDelta{Text: "response"})
		out.Emit(aitypes.EventStop{Reason: aitypes.StopReasonStop})
		out.Finish(nil)
	}()
	return out, nil
}

func (f fakeProvider) StreamSimple(model aitypes.Model, ctx aitypes.Context, opts aitypes.SimpleStreamOptions) (stream.AssistantEventStream, error) {
	return f.Stream(model, ctx, opts.StreamOptions)
}

func TestRunEmitsAssistantAndFinish(t *testing.T) {
	r := registry.New()
	r.Register(fakeProvider{})
	engine := &Engine{Providers: r}

	events := make([]agenttypes.Event, 0, 3)
	history, err := engine.Run(context.Background(), agenttypes.Config{Model: aitypes.Model{API: "fake", Name: "stub"}}, []aitypes.Message{aitypes.UserMessage{Content: "hello"}}, func(e agenttypes.Event) {
		events = append(events, e)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected history len 2, got %d", len(history))
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if _, ok := events[0].(agenttypes.AgentStarted); !ok {
		t.Fatalf("expected AgentStarted first")
	}
	if _, ok := events[1].(agenttypes.AssistantEmitted); !ok {
		t.Fatalf("expected AssistantEmitted second")
	}
	if _, ok := events[2].(agenttypes.AgentFinished); !ok {
		t.Fatalf("expected AgentFinished third")
	}
}

func TestContinueRequiresValidTail(t *testing.T) {
	engine := &Engine{}
	_, err := engine.Continue(context.Background(), agenttypes.Config{}, []aitypes.Message{aitypes.AssistantMessage{}}, nil)
	if err == nil {
		t.Fatalf("expected tail validation error")
	}
}
