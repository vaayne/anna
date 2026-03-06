package openairesponse

import (
	"fmt"
	"strings"

	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/responses"
	"github.com/vaayne/anna/pkg/ai/types"
)

func convertMessages(ctx types.Context) responses.ResponseInputParam {
	items := make(responses.ResponseInputParam, 0, len(ctx.Messages))

	for _, msg := range ctx.Messages {
		switch m := msg.(type) {
		case types.UserMessage:
			items = append(items, responses.ResponseInputItemUnionParam{
				OfMessage: &responses.EasyInputMessageParam{
					Role: responses.EasyInputMessageRoleUser,
					Content: responses.EasyInputMessageContentUnionParam{
						OfString: param.NewOpt(userContent(m.Content)),
					},
				},
			})
		case types.AssistantMessage:
			items = append(items, responses.ResponseInputItemUnionParam{
				OfMessage: &responses.EasyInputMessageParam{
					Role: responses.EasyInputMessageRoleAssistant,
					Content: responses.EasyInputMessageContentUnionParam{
						OfString: param.NewOpt(flattenAssistantContent(m.Content)),
					},
				},
			})
		case types.ToolResultMessage:
			items = append(items, responses.ResponseInputItemUnionParam{
				OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
					CallID: m.ToolCallID,
					Output: flattenToolResult(m.Content),
				},
			})
		}
	}
	return items
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
			// omit thinking from messages
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
