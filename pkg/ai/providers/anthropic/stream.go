package anthropic

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/vaayne/anna/pkg/ai/stream"
	"github.com/vaayne/anna/pkg/ai/types"
)

type eventEnvelope struct {
	Type  string          `json:"type"`
	Delta json.RawMessage `json:"delta"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	StopReason   string `json:"stop_reason"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
}

func parseEnvelope(eventType, data string) ([]types.AssistantEvent, error) {
	var envelope eventEnvelope
	if err := json.Unmarshal([]byte(data), &envelope); err != nil {
		return nil, fmt.Errorf("parse anthropic envelope: %w", err)
	}

	events := make([]types.AssistantEvent, 0, 3)
	switch eventType {
	case "message_start":
		events = append(events, types.EventStart{})
	case "content_block_start":
		if envelope.ContentBlock.Type == "tool_use" {
			events = append(events, types.EventToolCallDelta{ID: envelope.ContentBlock.ID, Name: envelope.ContentBlock.Name})
		}
	case "content_block_delta":
		var delta map[string]any
		_ = json.Unmarshal(envelope.Delta, &delta)
		if text, ok := delta["text"].(string); ok && text != "" {
			events = append(events, types.EventTextDelta{Text: text})
		}
		if thinking, ok := delta["thinking"].(string); ok && thinking != "" {
			events = append(events, types.EventThinkingDelta{Thinking: thinking})
		}
		if inputJSON, ok := delta["partial_json"].(string); ok && inputJSON != "" {
			events = append(events, types.EventToolCallDelta{Arguments: inputJSON})
		}
	case "message_delta":
		if envelope.StopReason != "" {
			events = append(events, types.EventStop{Reason: mapStopReason(envelope.StopReason)})
		}
		if envelope.Usage.InputTokens > 0 || envelope.Usage.OutputTokens > 0 {
			events = append(events, types.EventUsage{Usage: types.Usage{
				InputTokens:  envelope.Usage.InputTokens,
				OutputTokens: envelope.Usage.OutputTokens,
				TotalTokens:  envelope.Usage.InputTokens + envelope.Usage.OutputTokens,
			}})
		}
	case "message_stop":
		events = append(events, types.EventDone{})
	}

	return events, nil
}

func parseSSEReader(reader io.Reader, out *stream.ChannelEventStream) error {
	scanner := bufio.NewScanner(reader)
	var eventType string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		events, err := parseEnvelope(eventType, data)
		if err != nil {
			return err
		}
		for _, event := range events {
			out.Emit(event)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
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
