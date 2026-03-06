package openairesponse

import (
	"github.com/openai/openai-go/packages/ssestream"
	"github.com/openai/openai-go/responses"
	"github.com/vaayne/anna/pkg/ai/stream"
	"github.com/vaayne/anna/pkg/ai/types"
)

func consumeStream(sdkStream *ssestream.Stream[responses.ResponseStreamEventUnion], out *stream.ChannelEventStream) {
	// Track item_id → call_id so argument deltas use the correct call ID.
	itemToCall := make(map[string]string)

	for sdkStream.Next() {
		event := sdkStream.Current()
		for _, e := range mapEvent(event, itemToCall) {
			out.Emit(e)
		}
	}
}

func mapEvent(event responses.ResponseStreamEventUnion, itemToCall map[string]string) []types.AssistantEvent {
	var events []types.AssistantEvent

	switch event.Type {
	case "response.output_text.delta":
		if event.Delta.OfString != "" {
			events = append(events, types.EventTextDelta{Text: event.Delta.OfString})
		}

	case "response.output_item.added":
		if event.Item.Type == "function_call" {
			// Record item_id → call_id so argument deltas can resolve the call ID.
			itemToCall[event.Item.ID] = event.Item.CallID
			events = append(events, types.EventToolCallDelta{
				ID:   event.Item.CallID,
				Name: event.Item.Name,
			})
		}

	case "response.function_call_arguments.delta":
		callID := event.ItemID
		if mapped, ok := itemToCall[event.ItemID]; ok {
			callID = mapped
		}
		events = append(events, types.EventToolCallDelta{
			ID:        callID,
			Arguments: event.Delta.OfString,
		})

	case "response.function_call_arguments.done":
		// Arguments fully accumulated via deltas; nothing extra needed.

	case "response.completed":
		usage := event.Response.Usage
		if usage.TotalTokens > 0 || usage.InputTokens > 0 || usage.OutputTokens > 0 {
			events = append(events, types.EventUsage{Usage: types.Usage{
				InputTokens:  int(usage.InputTokens),
				OutputTokens: int(usage.OutputTokens),
				TotalTokens:  int(usage.TotalTokens),
			}})
		}
		events = append(events, types.EventStop{Reason: mapStopReason(event.Response.Status)})

	case "response.failed":
		events = append(events, types.EventStop{Reason: types.StopReasonError})

	case "response.incomplete":
		events = append(events, types.EventStop{Reason: types.StopReasonLength})
	}

	return events
}

func mapStopReason(status responses.ResponseStatus) types.StopReason {
	switch status {
	case responses.ResponseStatusCompleted:
		return types.StopReasonStop
	case responses.ResponseStatusIncomplete:
		return types.StopReasonLength
	case responses.ResponseStatusFailed:
		return types.StopReasonError
	default:
		return types.StopReasonStop
	}
}
