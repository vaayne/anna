package anthropic

import (
	"fmt"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/vaayne/anna/pkg/ai/types"
)

func convertMessages(ctx types.Context) []sdk.MessageParam {
	messages := make([]sdk.MessageParam, 0, len(ctx.Messages))
	for _, msg := range ctx.Messages {
		switch m := msg.(type) {
		case types.UserMessage:
			messages = append(messages, sdk.NewUserMessage(userContentBlocks(m.Content)...))
		case types.AssistantMessage:
			messages = append(messages, sdk.NewAssistantMessage(assistantContentBlocks(m.Content)...))
		case types.ToolResultMessage:
			messages = append(messages, sdk.NewUserMessage(toolResultBlock(m)))
		}
	}
	return messages
}

func userContentBlocks(content any) []sdk.ContentBlockParamUnion {
	switch c := content.(type) {
	case string:
		return []sdk.ContentBlockParamUnion{sdk.NewTextBlock(c)}
	case []types.ContentBlock:
		blocks := make([]sdk.ContentBlockParamUnion, 0, len(c))
		for _, block := range c {
			switch b := block.(type) {
			case types.TextContent:
				blocks = append(blocks, sdk.NewTextBlock(b.Text))
			}
		}
		return blocks
	default:
		return []sdk.ContentBlockParamUnion{sdk.NewTextBlock(fmt.Sprintf("%v", content))}
	}
}

func assistantContentBlocks(blocks []types.ContentBlock) []sdk.ContentBlockParamUnion {
	out := make([]sdk.ContentBlockParamUnion, 0, len(blocks))
	for _, block := range blocks {
		switch b := block.(type) {
		case types.TextContent:
			out = append(out, sdk.NewTextBlock(b.Text))
		case types.ThinkingContent:
			out = append(out, sdk.NewThinkingBlock(b.Signature, b.Thinking))
		case types.ToolCall:
			out = append(out, sdk.NewToolUseBlock(b.ID, b.Arguments, b.Name))
		}
	}
	return out
}

func toolResultBlock(m types.ToolResultMessage) sdk.ContentBlockParamUnion {
	text := ""
	for _, block := range m.Content {
		if t, ok := block.(types.TextContent); ok && t.Text != "" {
			text = t.Text
			break
		}
	}
	return sdk.NewToolResultBlock(m.ToolCallID, text, m.IsError)
}
