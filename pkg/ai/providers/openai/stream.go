package openai

import (
	sdk "github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/ssestream"
	"github.com/vaayne/anna/pkg/ai/stream"
	"github.com/vaayne/anna/pkg/ai/types"
)

func consumeStream(sdkStream *ssestream.Stream[sdk.ChatCompletionChunk], out *stream.ChannelEventStream) {
	for sdkStream.Next() {
		chunk := sdkStream.Current()
		for _, e := range mapChunk(chunk) {
			out.Emit(e)
		}
	}
}

func mapChunk(chunk sdk.ChatCompletionChunk) []types.AssistantEvent {
	var events []types.AssistantEvent

	for _, choice := range chunk.Choices {
		delta := choice.Delta
		if delta.Content != "" {
			events = append(events, types.EventTextDelta{Text: delta.Content})
		}
		for _, tc := range delta.ToolCalls {
			events = append(events, types.EventToolCallDelta{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
		if choice.FinishReason != "" {
			events = append(events, types.EventStop{Reason: mapStopReason(string(choice.FinishReason))})
		}
	}

	if chunk.Usage.TotalTokens > 0 || chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
		events = append(events, types.EventUsage{Usage: types.Usage{
			InputTokens:  int(chunk.Usage.PromptTokens),
			OutputTokens: int(chunk.Usage.CompletionTokens),
			TotalTokens:  int(chunk.Usage.TotalTokens),
		}})
	}

	return events
}

func mapStopReason(reason string) types.StopReason {
	switch reason {
	case "stop":
		return types.StopReasonStop
	case "length":
		return types.StopReasonLength
	case "tool_calls":
		return types.StopReasonToolUse
	default:
		return types.StopReasonStop
	}
}
