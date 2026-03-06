package openai

import (
	"testing"

	"github.com/vaayne/anna/pkg/ai/types"
)

func TestParseDataLineTextAndStop(t *testing.T) {
	data := `{"choices":[{"delta":{"content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`
	events, done, err := ParseDataLine(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Fatalf("expected done=false")
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if _, ok := events[0].(types.EventTextDelta); !ok {
		t.Fatalf("expected first event text delta")
	}
	if _, ok := events[1].(types.EventStop); !ok {
		t.Fatalf("expected second event stop")
	}
	if _, ok := events[2].(types.EventUsage); !ok {
		t.Fatalf("expected third event usage")
	}
}

func TestParseDataLineDone(t *testing.T) {
	events, done, err := ParseDataLine("[DONE]")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !done {
		t.Fatalf("expected done=true")
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if _, ok := events[0].(types.EventDone); !ok {
		t.Fatalf("expected done event")
	}
}
