package runner

import (
	"context"
	"testing"
)

func TestHandlerFunc(t *testing.T) {
	fn := HandlerFunc(func(ctx context.Context, history []RPCEvent, message string) <-chan Event {
		ch := make(chan Event, 1)
		ch <- Event{Text: "hello from handler: " + message}
		close(ch)
		return ch
	})

	// Verify it satisfies the Runner interface.
	var r Runner = fn
	stream := r.Chat(context.Background(), nil, "test")

	evt := <-stream
	if evt.Err != nil {
		t.Fatalf("unexpected error: %v", evt.Err)
	}
	if evt.Text != "hello from handler: test" {
		t.Errorf("text = %q, want %q", evt.Text, "hello from handler: test")
	}
}

func TestTextDeltaToRPCEvent(t *testing.T) {
	evt := TextDeltaToRPCEvent("hello")
	if evt.Type != "message_update" {
		t.Errorf("type = %q, want message_update", evt.Type)
	}
	if len(evt.AssistantMessageEvent) == 0 {
		t.Error("expected non-empty AssistantMessageEvent")
	}
}
