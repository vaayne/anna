package core

import (
	"context"
	"sync/atomic"
	"testing"

	agenttypes "github.com/vaayne/anna/pkg/agent/types"
	"github.com/vaayne/anna/pkg/ai/registry"
	"github.com/vaayne/anna/pkg/ai/stream"
	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

type fakeProvider struct {
	streamFunc func(model aitypes.Model, ctx aitypes.Context, opts aitypes.StreamOptions) (stream.AssistantEventStream, error)
}

func (f fakeProvider) API() string { return "fake" }

func (f fakeProvider) Stream(model aitypes.Model, ctx aitypes.Context, opts aitypes.StreamOptions) (stream.AssistantEventStream, error) {
	if f.streamFunc != nil {
		return f.streamFunc(model, ctx, opts)
	}
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

func newEngine(p fakeProvider) *Engine {
	r := registry.New()
	r.Register(p)
	return &Engine{Providers: r}
}

func collectEvents(engine *Engine, cfg agenttypes.Config, history []aitypes.Message) ([]aitypes.Message, []agenttypes.Event, error) {
	var events []agenttypes.Event
	h, err := engine.Run(context.Background(), cfg, history, func(e agenttypes.Event) {
		events = append(events, e)
	})
	return h, events, err
}

func countEvents[T agenttypes.Event](events []agenttypes.Event) int {
	n := 0
	for _, e := range events {
		if _, ok := e.(T); ok {
			n++
		}
	}
	return n
}

var baseCfg = agenttypes.Config{Model: aitypes.Model{API: "fake", Name: "stub"}}

func TestRunEmitsStreamingEvents(t *testing.T) {
	engine := newEngine(fakeProvider{})
	history, events, err := collectEvents(engine, baseCfg, []aitypes.Message{aitypes.UserMessage{Content: "hello"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected history len 2, got %d", len(history))
	}

	// Verify lifecycle: AgentStarted, TurnStarted, AssistantStarted, AssistantDelta(s), AssistantFinished, TurnFinished, AgentFinished
	if _, ok := events[0].(agenttypes.AgentStarted); !ok {
		t.Fatalf("expected AgentStarted first, got %T", events[0])
	}
	if _, ok := events[1].(agenttypes.TurnStarted); !ok {
		t.Fatalf("expected TurnStarted second, got %T", events[1])
	}
	if _, ok := events[2].(agenttypes.AssistantStarted); !ok {
		t.Fatalf("expected AssistantStarted third, got %T", events[2])
	}

	// Should have deltas for TextDelta and Stop events
	if countEvents[agenttypes.AssistantDelta](events) < 1 {
		t.Fatalf("expected at least 1 AssistantDelta")
	}

	// Last 3 should be AssistantFinished, TurnFinished, AgentFinished
	n := len(events)
	if _, ok := events[n-3].(agenttypes.AssistantFinished); !ok {
		t.Fatalf("expected AssistantFinished at n-3, got %T", events[n-3])
	}
	if _, ok := events[n-2].(agenttypes.TurnFinished); !ok {
		t.Fatalf("expected TurnFinished at n-2, got %T", events[n-2])
	}
	if _, ok := events[n-1].(agenttypes.AgentFinished); !ok {
		t.Fatalf("expected AgentFinished at n-1, got %T", events[n-1])
	}

	// Verify final message text
	finished := events[n-3].(agenttypes.AssistantFinished)
	if len(finished.Message.Content) == 0 {
		t.Fatalf("expected content in final message")
	}
	tc, ok := finished.Message.Content[0].(aitypes.TextContent)
	if !ok || tc.Text != "response" {
		t.Fatalf("expected text 'response', got %v", finished.Message.Content[0])
	}
}

func TestRunStreamingDeltasCarryPartial(t *testing.T) {
	provider := fakeProvider{
		streamFunc: func(_ aitypes.Model, _ aitypes.Context, _ aitypes.StreamOptions) (stream.AssistantEventStream, error) {
			out := stream.NewChannelEventStream(8)
			go func() {
				out.Emit(aitypes.EventTextDelta{Text: "hello "})
				out.Emit(aitypes.EventTextDelta{Text: "world"})
				out.Emit(aitypes.EventStop{Reason: aitypes.StopReasonStop})
				out.Finish(nil)
			}()
			return out, nil
		},
	}

	engine := newEngine(provider)
	_, events, err := collectEvents(engine, baseCfg, []aitypes.Message{aitypes.UserMessage{Content: "go"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Find the second text delta — should have accumulated text
	deltaCount := 0
	for _, e := range events {
		d, ok := e.(agenttypes.AssistantDelta)
		if !ok {
			continue
		}
		if _, isText := d.Event.(aitypes.EventTextDelta); !isText {
			continue
		}
		deltaCount++
		if deltaCount == 2 {
			if len(d.Message.Content) == 0 {
				t.Fatalf("expected partial content in second delta")
			}
			tc, ok := d.Message.Content[0].(aitypes.TextContent)
			if !ok || tc.Text != "hello world" {
				t.Fatalf("expected accumulated text 'hello world', got %v", d.Message.Content)
			}
		}
	}
	if deltaCount < 2 {
		t.Fatalf("expected at least 2 text deltas, got %d", deltaCount)
	}
}

func TestRunMultiTurnLoop(t *testing.T) {
	var callCount atomic.Int32

	provider := fakeProvider{
		streamFunc: func(_ aitypes.Model, _ aitypes.Context, _ aitypes.StreamOptions) (stream.AssistantEventStream, error) {
			out := stream.NewChannelEventStream(8)
			n := callCount.Add(1)
			go func() {
				if n <= 2 {
					out.Emit(aitypes.EventToolCallDelta{ID: "call_" + string(rune('0'+n)), Name: "test_tool", Arguments: "{}"})
					out.Emit(aitypes.EventStop{Reason: aitypes.StopReasonToolUse})
				} else {
					out.Emit(aitypes.EventTextDelta{Text: "done"})
					out.Emit(aitypes.EventStop{Reason: aitypes.StopReasonStop})
				}
				out.Finish(nil)
			}()
			return out, nil
		},
	}

	engine := newEngine(provider)
	cfg := agenttypes.Config{
		Model: aitypes.Model{API: "fake", Name: "stub"},
		Tools: agenttypes.ToolSet{
			"test_tool": func(_ context.Context, _ aitypes.ToolCall) (aitypes.TextContent, error) {
				return aitypes.TextContent{Text: "tool result"}, nil
			},
		},
	}

	history, events, err := collectEvents(engine, cfg, []aitypes.Message{aitypes.UserMessage{Content: "go"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// history: user + (assistant + tool_result) * 2 + assistant = 6
	if len(history) != 6 {
		t.Fatalf("expected history len 6, got %d", len(history))
	}

	if countEvents[agenttypes.TurnStarted](events) != 3 {
		t.Fatalf("expected 3 TurnStarted, got %d", countEvents[agenttypes.TurnStarted](events))
	}
	if countEvents[agenttypes.TurnFinished](events) != 3 {
		t.Fatalf("expected 3 TurnFinished, got %d", countEvents[agenttypes.TurnFinished](events))
	}
	if countEvents[agenttypes.AssistantStarted](events) != 3 {
		t.Fatalf("expected 3 AssistantStarted, got %d", countEvents[agenttypes.AssistantStarted](events))
	}
	if countEvents[agenttypes.AssistantFinished](events) != 3 {
		t.Fatalf("expected 3 AssistantFinished, got %d", countEvents[agenttypes.AssistantFinished](events))
	}
}

func TestRunMaxTurnsEnforced(t *testing.T) {
	provider := fakeProvider{
		streamFunc: func(_ aitypes.Model, _ aitypes.Context, _ aitypes.StreamOptions) (stream.AssistantEventStream, error) {
			out := stream.NewChannelEventStream(8)
			go func() {
				out.Emit(aitypes.EventToolCallDelta{ID: "call_1", Name: "test_tool", Arguments: "{}"})
				out.Emit(aitypes.EventStop{Reason: aitypes.StopReasonToolUse})
				out.Finish(nil)
			}()
			return out, nil
		},
	}

	engine := newEngine(provider)
	cfg := agenttypes.Config{
		Model:    aitypes.Model{API: "fake", Name: "stub"},
		MaxTurns: 2,
		Tools: agenttypes.ToolSet{
			"test_tool": func(_ context.Context, _ aitypes.ToolCall) (aitypes.TextContent, error) {
				return aitypes.TextContent{Text: "ok"}, nil
			},
		},
	}

	_, _, err := collectEvents(engine, cfg, []aitypes.Message{aitypes.UserMessage{Content: "go"}})
	if err == nil {
		t.Fatalf("expected max turns error")
	}
}

func TestRunStopsOnErrorStopReason(t *testing.T) {
	provider := fakeProvider{
		streamFunc: func(_ aitypes.Model, _ aitypes.Context, _ aitypes.StreamOptions) (stream.AssistantEventStream, error) {
			out := stream.NewChannelEventStream(8)
			go func() {
				out.Emit(aitypes.EventError{Err: nil})
				out.Emit(aitypes.EventStop{Reason: aitypes.StopReasonError})
				out.Finish(nil)
			}()
			return out, nil
		},
	}

	engine := newEngine(provider)
	history, _, err := collectEvents(engine, baseCfg, []aitypes.Message{aitypes.UserMessage{Content: "go"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected history len 2, got %d", len(history))
	}
}

func TestRunInterruptStopsLoop(t *testing.T) {
	var callCount atomic.Int32
	interrupt := make(chan struct{})

	provider := fakeProvider{
		streamFunc: func(_ aitypes.Model, _ aitypes.Context, _ aitypes.StreamOptions) (stream.AssistantEventStream, error) {
			out := stream.NewChannelEventStream(8)
			n := callCount.Add(1)
			go func() {
				out.Emit(aitypes.EventToolCallDelta{ID: "call_1", Name: "test_tool", Arguments: "{}"})
				out.Emit(aitypes.EventStop{Reason: aitypes.StopReasonToolUse})
				out.Finish(nil)
			}()
			if n == 1 {
				close(interrupt)
			}
			return out, nil
		},
	}

	engine := newEngine(provider)
	cfg := agenttypes.Config{
		Model:     aitypes.Model{API: "fake", Name: "stub"},
		Interrupt: interrupt,
		Tools: agenttypes.ToolSet{
			"test_tool": func(_ context.Context, _ aitypes.ToolCall) (aitypes.TextContent, error) {
				return aitypes.TextContent{Text: "ok"}, nil
			},
		},
	}

	history, _, err := collectEvents(engine, cfg, []aitypes.Message{aitypes.UserMessage{Content: "go"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected history len 2, got %d", len(history))
	}
}

func TestContinueRequiresValidTail(t *testing.T) {
	engine := &Engine{}
	_, err := engine.Continue(context.Background(), agenttypes.Config{}, []aitypes.Message{aitypes.AssistantMessage{}}, nil)
	if err == nil {
		t.Fatalf("expected tail validation error")
	}
}
