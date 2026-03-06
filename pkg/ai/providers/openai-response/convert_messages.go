package openairesponse

import (
	"encoding/json"
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
			items = append(items, convertAssistantMessage(m)...)
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

func convertAssistantMessage(m types.AssistantMessage) responses.ResponseInputParam {
	var items responses.ResponseInputParam
	var textParts []string

	for _, block := range m.Content {
		switch b := block.(type) {
		case types.TextContent:
			textParts = append(textParts, b.Text)
		case types.ToolCall:
			argsJSON, _ := json.Marshal(b.Arguments)
			items = append(items, responses.ResponseInputItemUnionParam{
				OfFunctionCall: &responses.ResponseFunctionToolCallParam{
					Arguments: string(argsJSON),
					CallID:    b.ID,
					Name:      b.Name,
				},
			})
		}
	}

	if text := strings.Join(textParts, " "); text != "" {
		items = append([]responses.ResponseInputItemUnionParam{{
			OfMessage: &responses.EasyInputMessageParam{
				Role: responses.EasyInputMessageRoleAssistant,
				Content: responses.EasyInputMessageContentUnionParam{
					OfString: param.NewOpt(text),
				},
			},
		}}, items...)
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

func flattenToolResult(content []types.ContentBlock) string {
	parts := make([]string, 0, len(content))
	for _, block := range content {
		if t, ok := block.(types.TextContent); ok && t.Text != "" {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, " ")
}
