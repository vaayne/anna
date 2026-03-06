package openai

import (
	"testing"

	sdk "github.com/openai/openai-go"
	"github.com/vaayne/anna/pkg/ai/types"
)

func TestMapChunkTextAndStop(t *testing.T) {
	indexToID := make(map[int]string)
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
	events := mapChunk(chunk, indexToID)
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
	indexToID := make(map[int]string)
	chunk := sdk.ChatCompletionChunk{
		Choices: []sdk.ChatCompletionChunkChoice{
			{
				Delta: sdk.ChatCompletionChunkChoiceDelta{
					ToolCalls: []sdk.ChatCompletionChunkChoiceDeltaToolCall{
						{
							Index:    0,
							ID:       "call_0",
							Function: sdk.ChatCompletionChunkChoiceDeltaToolCallFunction{Name: "lookup", Arguments: `{"q":"test"}`},
						},
					},
				},
				FinishReason: "tool_calls",
			},
		},
	}
	events := mapChunk(chunk, indexToID)
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

func TestMapChunkToolCallIDCarriedForward(t *testing.T) {
	indexToID := make(map[int]string)

	// First chunk: carries the tool call ID and name.
	chunk1 := sdk.ChatCompletionChunk{
		Choices: []sdk.ChatCompletionChunkChoice{
			{
				Delta: sdk.ChatCompletionChunkChoiceDelta{
					ToolCalls: []sdk.ChatCompletionChunkChoiceDeltaToolCall{
						{Index: 0, ID: "call_abc", Function: sdk.ChatCompletionChunkChoiceDeltaToolCallFunction{Name: "read_file"}},
					},
				},
			},
		},
	}
	events := mapChunk(chunk1, indexToID)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	tc := events[0].(types.EventToolCallDelta)
	if tc.ID != "call_abc" || tc.Name != "read_file" {
		t.Fatalf("unexpected first delta: %+v", tc)
	}

	// Second chunk: no ID, only arguments at the same index.
	chunk2 := sdk.ChatCompletionChunk{
		Choices: []sdk.ChatCompletionChunkChoice{
			{
				Delta: sdk.ChatCompletionChunkChoiceDelta{
					ToolCalls: []sdk.ChatCompletionChunkChoiceDeltaToolCall{
						{Index: 0, Function: sdk.ChatCompletionChunkChoiceDeltaToolCallFunction{Arguments: `{"path":"/tmp"}`}},
					},
				},
			},
		},
	}
	events = mapChunk(chunk2, indexToID)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	tc = events[0].(types.EventToolCallDelta)
	if tc.ID != "call_abc" {
		t.Fatalf("expected ID 'call_abc' on argument delta, got %q", tc.ID)
	}
	if tc.Arguments != `{"path":"/tmp"}` {
		t.Fatalf("unexpected arguments: %q", tc.Arguments)
	}
}
