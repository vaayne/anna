package openai

import (
	"fmt"
	"strings"

	sdk "github.com/openai/openai-go"
	"github.com/vaayne/anna/pkg/ai/types"
)

func convertMessages(ctx types.Context) []sdk.ChatCompletionMessageParamUnion {
	messages := make([]sdk.ChatCompletionMessageParamUnion, 0, len(ctx.Messages)+1)

	if ctx.System != "" {
		messages = append(messages, sdk.SystemMessage(ctx.System))
	}

	for _, msg := range ctx.Messages {
		switch m := msg.(type) {
		case types.UserMessage:
			messages = append(messages, sdk.UserMessage(userContent(m.Content)))
		case types.AssistantMessage:
			messages = append(messages, sdk.AssistantMessage(flattenAssistantContent(m.Content)))
		case types.ToolResultMessage:
			messages = append(messages, sdk.ToolMessage(flattenToolResult(m.Content), m.ToolCallID))
		}
	}
	return messages
}

func userContent(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []types.ContentBlock:
		parts := make([]string, 0, len(c))
		for _, block := range c {
			if t, ok := block.(types.TextContent); ok && t.Text != "" {
				parts = append(parts, t.Text)
			}
		}
		return strings.Join(parts, " ")
	default:
		return fmt.Sprintf("%v", content)
	}
}

func flattenAssistantContent(blocks []types.ContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		switch b := block.(type) {
		case types.TextContent:
			parts = append(parts, b.Text)
		case types.ThinkingContent:
			// omit thinking from OpenAI messages
		case types.ToolCall:
			parts = append(parts, fmt.Sprintf("[tool_call:%s]", b.Name))
		}
	}
	return strings.Join(parts, " ")
}

func flattenToolResult(content []types.ContentBlock) string {
	parts := make([]string, 0, len(content))
	for _, block := range content {
		if t, ok := block.(types.TextContent); ok && t.Text != "" {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, " ")
}
