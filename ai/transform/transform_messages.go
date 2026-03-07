package transform

import (
	"time"

	"github.com/vaayne/anna/ai/types"
)

// Messages applies compatibility normalization needed by provider adapters.
func Messages(messages []types.Message) []types.Message {
	out := make([]types.Message, 0, len(messages)+2)
	now := time.Now().UTC()

	for i := range messages {
		msg := messages[i]
		out = append(out, msg)

		assistant, ok := msg.(types.AssistantMessage)
		if !ok {
			continue
		}

		for _, block := range assistant.Content {
			call, ok := block.(types.ToolCall)
			if !ok {
				continue
			}
			if hasToolResultFor(messages, i+1, call.ID) {
				continue
			}
			out = append(out, types.ToolResultMessage{
				ToolCallID: call.ID,
				ToolName:   call.Name,
				Content: []types.ContentBlock{
					types.TextContent{Text: "synthetic tool result: missing tool output"},
				},
				IsError:   true,
				Timestamp: now,
			})
		}
	}

	return out
}

func hasToolResultFor(messages []types.Message, start int, toolCallID string) bool {
	for i := start; i < len(messages); i++ {
		toolResult, ok := messages[i].(types.ToolResultMessage)
		if !ok {
			continue
		}
		if toolResult.ToolCallID == toolCallID {
			return true
		}
	}
	return false
}
