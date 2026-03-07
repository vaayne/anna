package transform

import (
	"testing"

	"github.com/vaayne/anna/ai/types"
)

func TestMessagesAddsSyntheticToolResultForOrphanToolCall(t *testing.T) {
	input := []types.Message{
		types.UserMessage{Content: "hi"},
		types.AssistantMessage{Content: []types.ContentBlock{
			types.ToolCall{ID: "tool-1", Name: "lookup"},
		}},
	}

	out := Messages(input)
	if len(out) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(out))
	}

	toolMsg, ok := out[2].(types.ToolResultMessage)
	if !ok {
		t.Fatalf("expected synthetic tool result message at index 2")
	}
	if !toolMsg.IsError {
		t.Fatalf("expected synthetic tool result to be marked error")
	}
	if toolMsg.ToolCallID != "tool-1" {
		t.Fatalf("expected toolCallID tool-1, got %q", toolMsg.ToolCallID)
	}
}

func TestMessagesKeepsExistingToolResult(t *testing.T) {
	input := []types.Message{
		types.AssistantMessage{Content: []types.ContentBlock{
			types.ToolCall{ID: "tool-1", Name: "lookup"},
		}},
		types.ToolResultMessage{ToolCallID: "tool-1", ToolName: "lookup"},
	}

	out := Messages(input)
	if len(out) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(out))
	}
}
