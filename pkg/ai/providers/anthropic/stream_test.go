package anthropic

import (
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/vaayne/anna/pkg/ai/types"
)

func TestMapEventContentDelta(t *testing.T) {
	event := sdk.MessageStreamEventUnion{
		Type: "content_block_delta",
		Delta: sdk.MessageStreamEventUnionDelta{
			Type: "text_delta",
			Text: "hello",
		},
	}
	events := mapEvent(event)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if _, ok := events[0].(types.EventTextDelta); !ok {
		t.Fatalf("expected text delta")
	}
}

func TestMapEventMessageDeltaUsage(t *testing.T) {
	event := sdk.MessageStreamEventUnion{
		Type: "message_delta",
		Delta: sdk.MessageStreamEventUnionDelta{
			StopReason: "end_turn",
		},
		Usage: sdk.MessageDeltaUsage{
			InputTokens:  8,
			OutputTokens: 2,
		},
	}
	events := mapEvent(event)
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
