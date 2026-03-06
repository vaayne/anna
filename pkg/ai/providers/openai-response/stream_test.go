package openairesponse

import (
	"testing"

	"github.com/openai/openai-go/responses"
	"github.com/vaayne/anna/pkg/ai/types"
)

func newItemToCall() map[string]string { return make(map[string]string) }

func TestMapEventTextDelta(t *testing.T) {
	event := responses.ResponseStreamEventUnion{
		Type: "response.output_text.delta",
		Delta: responses.ResponseStreamEventUnionDelta{
			OfString: "hello",
		},
	}
	events := mapEvent(event, newItemToCall())
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
	// Simulate the real flow: output_item.added registers the item_id → call_id mapping,
	// then arguments.delta uses item_id which gets resolved to call_id.
	m := newItemToCall()
	m["fc_0"] = "call_0"

	event := responses.ResponseStreamEventUnion{
		Type:   "response.function_call_arguments.delta",
		ItemID: "fc_0",
		Delta: responses.ResponseStreamEventUnionDelta{
			OfString: `{"q":"test"}`,
		},
	}
	events := mapEvent(event, m)
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
	m := newItemToCall()
	event := responses.ResponseStreamEventUnion{
		Type: "response.output_item.added",
		Item: responses.ResponseOutputItemUnion{
			ID:     "fc_1",
			Type:   "function_call",
			CallID: "call_1",
			Name:   "lookup",
		},
	}
	events := mapEvent(event, m)
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
	// Verify the item_id → call_id mapping was recorded.
	if m["fc_1"] != "call_1" {
		t.Fatalf("expected item_id mapping fc_1→call_1, got %q", m["fc_1"])
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
	events := mapEvent(event, newItemToCall())
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
	events := mapEvent(event, newItemToCall())
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

func TestMapEventFunctionCallFlow(t *testing.T) {
	// Simulate the full function call streaming sequence:
	// 1. output_item.added (registers item_id → call_id)
	// 2. function_call_arguments.delta (uses item_id, resolved to call_id)
	m := newItemToCall()

	// Step 1: output_item.added
	added := responses.ResponseStreamEventUnion{
		Type: "response.output_item.added",
		Item: responses.ResponseOutputItemUnion{
			ID:     "fc_abc",
			Type:   "function_call",
			CallID: "call_abc",
			Name:   "bash",
		},
	}
	events := mapEvent(added, m)
	if len(events) != 1 {
		t.Fatalf("step 1: expected 1 event, got %d", len(events))
	}
	tc := events[0].(types.EventToolCallDelta)
	if tc.ID != "call_abc" || tc.Name != "bash" {
		t.Fatalf("step 1: unexpected: %+v", tc)
	}

	// Step 2: arguments delta (item_id=fc_abc should resolve to call_abc)
	delta := responses.ResponseStreamEventUnion{
		Type:   "response.function_call_arguments.delta",
		ItemID: "fc_abc",
		Delta: responses.ResponseStreamEventUnionDelta{
			OfString: `{"command":"ls"}`,
		},
	}
	events = mapEvent(delta, m)
	if len(events) != 1 {
		t.Fatalf("step 2: expected 1 event, got %d", len(events))
	}
	tc = events[0].(types.EventToolCallDelta)
	if tc.ID != "call_abc" {
		t.Fatalf("step 2: expected ID call_abc, got %q", tc.ID)
	}
	if tc.Arguments != `{"command":"ls"}` {
		t.Fatalf("step 2: unexpected arguments: %q", tc.Arguments)
	}
}

func TestMapEventIncomplete(t *testing.T) {
	event := responses.ResponseStreamEventUnion{
		Type: "response.incomplete",
	}
	events := mapEvent(event, newItemToCall())
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
