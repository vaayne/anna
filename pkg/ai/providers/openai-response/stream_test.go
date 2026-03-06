package openairesponse

import (
	"testing"

	"github.com/openai/openai-go/responses"
	"github.com/vaayne/anna/pkg/ai/types"
)

func TestMapEventTextDelta(t *testing.T) {
	event := responses.ResponseStreamEventUnion{
		Type: "response.output_text.delta",
		Delta: responses.ResponseStreamEventUnionDelta{
			OfString: "hello",
		},
	}
	events := mapEvent(event)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	td, ok := events[0].(types.EventTextDelta)
	if !ok {
		t.Fatalf("expected EventTextDelta, got %T", events[0])
	}
	if td.Text != "hello" {
		t.Fatalf("expected text 'hello', got %q", td.Text)
	}
}

func TestMapEventFunctionCallArgumentsDelta(t *testing.T) {
	event := responses.ResponseStreamEventUnion{
		Type:   "response.function_call_arguments.delta",
		ItemID: "call_0",
		Delta: responses.ResponseStreamEventUnionDelta{
			OfString: `{"q":"test"}`,
		},
	}
	events := mapEvent(event)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	tc, ok := events[0].(types.EventToolCallDelta)
	if !ok {
		t.Fatalf("expected EventToolCallDelta, got %T", events[0])
	}
	if tc.ID != "call_0" || tc.Arguments != `{"q":"test"}` {
		t.Fatalf("unexpected tool call delta: %+v", tc)
	}
}

func TestMapEventOutputItemAddedFunctionCall(t *testing.T) {
	event := responses.ResponseStreamEventUnion{
		Type: "response.output_item.added",
		Item: responses.ResponseOutputItemUnion{
			Type:   "function_call",
			CallID: "call_1",
			Name:   "lookup",
		},
	}
	events := mapEvent(event)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	tc, ok := events[0].(types.EventToolCallDelta)
	if !ok {
		t.Fatalf("expected EventToolCallDelta, got %T", events[0])
	}
	if tc.ID != "call_1" || tc.Name != "lookup" {
		t.Fatalf("unexpected tool call: %+v", tc)
	}
}

func TestMapEventCompleted(t *testing.T) {
	event := responses.ResponseStreamEventUnion{
		Type: "response.completed",
		Response: responses.Response{
			Status: responses.ResponseStatusCompleted,
			Usage: responses.ResponseUsage{
				InputTokens:  10,
				OutputTokens: 5,
				TotalTokens:  15,
			},
		},
	}
	events := mapEvent(event)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	usage, ok := events[0].(types.EventUsage)
	if !ok {
		t.Fatalf("expected EventUsage, got %T", events[0])
	}
	if usage.Usage.InputTokens != 10 || usage.Usage.OutputTokens != 5 || usage.Usage.TotalTokens != 15 {
		t.Fatalf("unexpected usage: %+v", usage.Usage)
	}
	stop, ok := events[1].(types.EventStop)
	if !ok {
		t.Fatalf("expected EventStop, got %T", events[1])
	}
	if stop.Reason != types.StopReasonStop {
		t.Fatalf("expected stop reason 'stop', got %q", stop.Reason)
	}
}

func TestMapEventFailed(t *testing.T) {
	event := responses.ResponseStreamEventUnion{
		Type: "response.failed",
	}
	events := mapEvent(event)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	stop, ok := events[0].(types.EventStop)
	if !ok {
		t.Fatalf("expected EventStop, got %T", events[0])
	}
	if stop.Reason != types.StopReasonError {
		t.Fatalf("expected stop reason error, got %q", stop.Reason)
	}
}

func TestMapEventIncomplete(t *testing.T) {
	event := responses.ResponseStreamEventUnion{
		Type: "response.incomplete",
	}
	events := mapEvent(event)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	stop, ok := events[0].(types.EventStop)
	if !ok {
		t.Fatalf("expected EventStop, got %T", events[0])
	}
	if stop.Reason != types.StopReasonLength {
		t.Fatalf("expected stop reason length, got %q", stop.Reason)
	}
}
