package proxy

import (
	"testing"

	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	raw, err := EncodeEvent(aitypes.EventTextDelta{Text: "hello"})
	if err != nil {
		t.Fatalf("unexpected encode error: %v", err)
	}
	event, err := DecodeEvent(raw)
	if err != nil {
		t.Fatalf("unexpected decode error: %v", err)
	}
	text, ok := event.(aitypes.EventTextDelta)
	if !ok || text.Text != "hello" {
		t.Fatalf("unexpected event: %#v", event)
	}
}
