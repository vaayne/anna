package core

import (
	"context"
	"errors"
	"testing"

	agenttypes "github.com/vaayne/anna/pkg/agent/types"
	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

func TestExecuteToolCalls(t *testing.T) {
	calls := []aitypes.ToolCall{{ID: "1", Name: "echo"}, {ID: "2", Name: "missing"}}
	tools := agenttypes.ToolSet{
		"echo": func(ctx context.Context, call aitypes.ToolCall) (aitypes.TextContent, error) {
			return aitypes.TextContent{Text: "ok"}, nil
		},
	}

	results, err := ExecuteToolCalls(context.Background(), calls, tools, ToolCallbacks{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].IsError {
		t.Fatalf("expected first result success")
	}
	if !results[1].IsError {
		t.Fatalf("expected second result error for missing tool")
	}
}

func TestExecuteToolCallsToolError(t *testing.T) {
	calls := []aitypes.ToolCall{{ID: "1", Name: "fail"}}
	tools := agenttypes.ToolSet{
		"fail": func(ctx context.Context, call aitypes.ToolCall) (aitypes.TextContent, error) {
			return aitypes.TextContent{}, errors.New("boom")
		},
	}

	results, err := ExecuteToolCalls(context.Background(), calls, tools, ToolCallbacks{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("expected error result")
	}
}
