package anthropic

import (
	"testing"

	"github.com/vaayne/anna/pkg/ai/types"
)

func TestParseEnvelopeContentDelta(t *testing.T) {
	events, err := parseEnvelope("content_block_delta", `{"delta":{"text":"hello"}}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if _, ok := events[0].(types.EventTextDelta); !ok {
		t.Fatalf("expected text delta")
	}
}

func TestParseEnvelopeMessageDeltaUsage(t *testing.T) {
	events, err := parseEnvelope("message_delta", `{"stop_reason":"end_turn","usage":{"input_tokens":8,"output_tokens":2}}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if _, ok := events[0].(types.EventStop); !ok {
		t.Fatalf("expected stop event")
	}
	if _, ok := events[1].(types.EventUsage); !ok {
		t.Fatalf("expected usage event")
	}
}
