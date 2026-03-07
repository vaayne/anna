package runner

import (
	"context"
	"errors"

	aitypes "github.com/vaayne/anna/ai/types"
)

// ToolCallbacks emits progress events around tool execution.
type ToolCallbacks struct {
	OnStart  func(call aitypes.ToolCall)
	OnFinish func(result aitypes.ToolResultMessage)
}

// ExecuteToolCalls runs each tool call in order and returns result messages.
func ExecuteToolCalls(ctx context.Context, calls []aitypes.ToolCall, tools ToolSet, cb ToolCallbacks) ([]aitypes.ToolResultMessage, error) {
	results := make([]aitypes.ToolResultMessage, 0, len(calls))

	for _, call := range calls {
		if cb.OnStart != nil {
			cb.OnStart(call)
		}

		toolFn, ok := tools[call.Name]
		if !ok {
			result := aitypes.ToolResultMessage{
				ToolCallID: call.ID,
				ToolName:   call.Name,
				IsError:    true,
				Content:    []aitypes.ContentBlock{aitypes.TextContent{Text: "tool not found"}},
			}
			results = append(results, result)
			if cb.OnFinish != nil {
				cb.OnFinish(result)
			}
			continue
		}

		content, err := toolFn(ctx, call)
		result := aitypes.ToolResultMessage{ToolCallID: call.ID, ToolName: call.Name, Content: []aitypes.ContentBlock{content}}
		if err != nil {
			result.IsError = true
			result.Content = []aitypes.ContentBlock{aitypes.TextContent{Text: err.Error()}}
		}
		results = append(results, result)
		if cb.OnFinish != nil {
			cb.OnFinish(result)
		}
	}

	if len(calls) > 0 && len(results) == 0 {
		return nil, errors.New("tool execution produced no results")
	}

	return results, nil
}
