package anthropic

import "github.com/vaayne/anna/pkg/ai/types"

// ConvertMessages maps normalized transcript to Anthropic message format.
func ConvertMessages(ctx types.Context) []any {
	messages := make([]any, 0, len(ctx.Messages))
	for _, msg := range ctx.Messages {
		switch m := msg.(type) {
		case types.UserMessage:
			messages = append(messages, map[string]any{"role": "user", "content": m.Content})
		case types.AssistantMessage:
			messages = append(messages, map[string]any{"role": "assistant", "content": flattenContentBlocks(m.Content)})
		case types.ToolResultMessage:
			messages = append(messages, map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type":        "tool_result",
					"tool_use_id": m.ToolCallID,
					"content":     m.Content,
					"is_error":    m.IsError,
				}},
			})
		}
	}
	return messages
}

func flattenContentBlocks(blocks []types.ContentBlock) []any {
	out := make([]any, 0, len(blocks))
	for _, block := range blocks {
		switch b := block.(type) {
		case types.TextContent:
			out = append(out, map[string]any{"type": "text", "text": b.Text})
		case types.ThinkingContent:
			out = append(out, map[string]any{"type": "thinking", "thinking": b.Thinking})
		case types.ToolCall:
			out = append(out, map[string]any{"type": "tool_use", "id": b.ID, "name": b.Name, "input": b.Arguments})
		}
	}
	return out
}
