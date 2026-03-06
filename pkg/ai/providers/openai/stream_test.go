package openai

import (
	"testing"

	sdk "github.com/openai/openai-go"
	"github.com/vaayne/anna/pkg/ai/types"
)

func TestMapChunkTextAndStop(t *testing.T) {
	chunk := sdk.ChatCompletionChunk{
		Choices: []sdk.ChatCompletionChunkChoice{
			{
				Delta:        sdk.ChatCompletionChunkChoiceDelta{Content: "hello"},
				FinishReason: "stop",
			},
		},
		Usage: sdk.CompletionUsage{
			PromptTokens:     5,
			CompletionTokens: 1,
			TotalTokens:      6,
		},
	}
	events := mapChunk(chunk)
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if _, ok := events[0].(types.EventTextDelta); !ok {
		t.Fatalf("expected first event text delta")
	}
	if _, ok := events[1].(types.EventStop); !ok {
		t.Fatalf("expected second event stop")
	}
	if _, ok := events[2].(types.EventUsage); !ok {
		t.Fatalf("expected third event usage")
	}
}

func TestMapChunkToolCalls(t *testing.T) {
	chunk := sdk.ChatCompletionChunk{
		Choices: []sdk.ChatCompletionChunkChoice{
			{
				Delta: sdk.ChatCompletionChunkChoiceDelta{
					ToolCalls: []sdk.ChatCompletionChunkChoiceDeltaToolCall{
						{
							ID:       "call_0",
							Function: sdk.ChatCompletionChunkChoiceDeltaToolCallFunction{Name: "lookup", Arguments: `{"q":"test"}`},
						},
					},
				},
				FinishReason: "tool_calls",
			},
		},
	}
	events := mapChunk(chunk)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	tc, ok := events[0].(types.EventToolCallDelta)
	if !ok {
		t.Fatalf("expected tool call delta")
	}
	if tc.ID != "call_0" || tc.Name != "lookup" {
		t.Fatalf("unexpected tool call: %+v", tc)
	}
	if _, ok := events[1].(types.EventStop); !ok {
		t.Fatalf("expected stop event")
	}
}
