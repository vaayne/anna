package anthropic

import (
	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/vaayne/anna/pkg/ai/stream"
	"github.com/vaayne/anna/pkg/ai/types"
)

func consumeStream(sdkStream *ssestream.Stream[sdk.MessageStreamEventUnion], out *stream.ChannelEventStream) {
	// Track content_block_index → tool_call_id so argument deltas carry the correct ID.
	blockToID := make(map[int]string)

	for sdkStream.Next() {
		event := sdkStream.Current()
		for _, e := range mapEvent(event, blockToID) {
			out.Emit(e)
		}
	}
}

func mapEvent(event sdk.MessageStreamEventUnion, blockToID map[int]string) []types.AssistantEvent {
	switch event.Type {
	case "message_start":
		return []types.AssistantEvent{types.EventStart{}}

	case "content_block_start":
		cb := event.ContentBlock
		if cb.Type == "tool_use" {
			blockToID[int(event.Index)] = cb.ID
			return []types.AssistantEvent{types.EventToolCallDelta{ID: cb.ID, Name: cb.Name}}
		}
		return nil

	case "content_block_delta":
		delta := event.Delta
		var events []types.AssistantEvent
		switch delta.Type {
		case "text_delta":
			if delta.Text != "" {
				events = append(events, types.EventTextDelta{Text: delta.Text})
			}
		case "thinking_delta":
			if delta.Thinking != "" {
				events = append(events, types.EventThinkingDelta{Thinking: delta.Thinking})
			}
		case "input_json_delta":
			if delta.PartialJSON != "" {
				events = append(events, types.EventToolCallDelta{
					ID:        blockToID[int(event.Index)],
					Arguments: delta.PartialJSON,
				})
			}
		}
		return events

	case "message_delta":
		var events []types.AssistantEvent
		if event.Delta.StopReason != "" {
			events = append(events, types.EventStop{Reason: mapStopReason(string(event.Delta.StopReason))})
		}
		if event.Usage.InputTokens > 0 || event.Usage.OutputTokens > 0 {
			events = append(events, types.EventUsage{Usage: types.Usage{
				InputTokens:  int(event.Usage.InputTokens),
				OutputTokens: int(event.Usage.OutputTokens),
				TotalTokens:  int(event.Usage.InputTokens + event.Usage.OutputTokens),
			}})
		}
		return events

	case "message_stop":
		return []types.AssistantEvent{types.EventDone{}}
	}

	return nil
}

func mapStopReason(reason string) types.StopReason {
	switch reason {
	case "tool_use":
		return types.StopReasonToolUse
	case "max_tokens":
		return types.StopReasonLength
	case "end_turn":
		return types.StopReasonStop
	default:
		return types.StopReasonStop
	}
}
