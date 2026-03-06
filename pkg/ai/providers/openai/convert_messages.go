package openai

import (
	"fmt"
	"strings"

	"github.com/vaayne/anna/pkg/ai/types"
)

// ConvertMessagesToPrompt flattens normalized messages into a completions prompt.
func ConvertMessagesToPrompt(ctx types.Context) string {
	parts := make([]string, 0, len(ctx.Messages)+1)
	if ctx.System != "" {
		parts = append(parts, fmt.Sprintf("system: %s", ctx.System))
	}

	for _, msg := range ctx.Messages {
		switch m := msg.(type) {
		case types.UserMessage:
			parts = append(parts, fmt.Sprintf("user: %v", m.Content))
		case types.AssistantMessage:
			parts = append(parts, fmt.Sprintf("assistant: %s", flattenAssistantContent(m.Content)))
		case types.ToolResultMessage:
			parts = append(parts, fmt.Sprintf("tool[%s]: %s", m.ToolName, flattenToolResult(m.Content)))
		case types.SystemMessage:
			parts = append(parts, fmt.Sprintf("system: %s", m.Content))
		}
	}

	return strings.Join(parts, "\n")
}

func flattenAssistantContent(blocks []types.ContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		switch b := block.(type) {
		case types.TextContent:
			parts = append(parts, b.Text)
		case types.ThinkingContent:
			parts = append(parts, "[thinking omitted]")
		case types.ToolCall:
			parts = append(parts, fmt.Sprintf("[tool_call:%s]", b.Name))
		}
	}
	return strings.Join(parts, " ")
}

func flattenToolResult(content []types.ToolResultContent) string {
	parts := make([]string, 0, len(content))
	for _, c := range content {
		if c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, " ")
}
