package gorunner

import (
	"context"
	"fmt"
	"testing"

	"github.com/vaayne/anna/agent/runner"
	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

func TestNewRequiresConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"missing api", Config{Model: "m", APIKey: "k"}},
		{"missing model", Config{API: "anthropic", APIKey: "k"}},
		{"missing api_key", Config{API: "anthropic", Model: "m"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(context.Background(), tt.cfg)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestNewSuccess(t *testing.T) {
	r, err := New(context.Background(), Config{
		API:    "anthropic",
		Model:  "claude-sonnet-4-20250514",
		APIKey: "test-key",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.Alive() {
		t.Error("new runner should be alive")
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestConvertHistoryEmpty(t *testing.T) {
	msgs := convertHistory(nil)
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages, got %d", len(msgs))
	}
}

func TestConvertHistoryRoundTrip(t *testing.T) {
	events := []runner.RPCEvent{
		{Type: "user_message", Summary: "hello"},
		runner.TextDeltaToRPCEvent("Hi "),
		runner.TextDeltaToRPCEvent("there!"),
		{Type: "user_message", Summary: "how are you?"},
		runner.TextDeltaToRPCEvent("I'm fine."),
	}

	msgs := convertHistory(events)

	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}

	// Verify message order and types.
	expected := []string{"user", "assistant", "user", "assistant"}
	for i, msg := range msgs {
		got := messageType(msg)
		if got != expected[i] {
			t.Errorf("message %d: type = %q, want %q", i, got, expected[i])
		}
	}
}

func TestConvertHistoryOnlyAssistant(t *testing.T) {
	events := []runner.RPCEvent{
		runner.TextDeltaToRPCEvent("orphan text"),
	}

	msgs := convertHistory(events)

	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if messageType(msgs[0]) != "assistant" {
		t.Errorf("expected assistant, got %q", messageType(msgs[0]))
	}
}

func messageType(msg aitypes.Message) string {
	switch msg.(type) {
	case aitypes.UserMessage:
		return "user"
	case aitypes.AssistantMessage:
		return "assistant"
	case aitypes.ToolResultMessage:
		return "tool"
	case aitypes.SystemMessage:
		return "system"
	default:
		return fmt.Sprintf("unknown(%T)", msg)
	}
}

func TestConvertHistorySkipsUnknownTypes(t *testing.T) {
	events := []runner.RPCEvent{
		{Type: "user_message", Summary: "hi"},
		{Type: "agent_end"},
		{Type: "error", Error: "something"},
		runner.TextDeltaToRPCEvent("reply"),
	}

	msgs := convertHistory(events)

	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
}
