package openai

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/vaayne/anna/pkg/ai/stream"
	"github.com/vaayne/anna/pkg/ai/types"
)

type completionChunk struct {
	Choices []struct {
		Delta struct {
			Content      string `json:"content"`
			FunctionCall struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function_call"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// ParseDataLine parses one SSE data line.
func ParseDataLine(data string) ([]types.AssistantEvent, bool, error) {
	if strings.TrimSpace(data) == "[DONE]" {
		return []types.AssistantEvent{types.EventDone{}}, true, nil
	}

	var chunk completionChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil, false, fmt.Errorf("parse openai chunk: %w", err)
	}

	events := make([]types.AssistantEvent, 0, 4)
	for _, choice := range chunk.Choices {
		if choice.Delta.Content != "" {
			events = append(events, types.EventTextDelta{Text: choice.Delta.Content})
		}
		if choice.Delta.FunctionCall.Name != "" || choice.Delta.FunctionCall.Arguments != "" {
			events = append(events, types.EventToolCallDelta{
				ID:        "call_0",
				Name:      choice.Delta.FunctionCall.Name,
				Arguments: choice.Delta.FunctionCall.Arguments,
			})
		}
		if choice.FinishReason != "" {
			events = append(events, types.EventStop{Reason: mapStopReason(choice.FinishReason)})
		}
	}

	if chunk.Usage.TotalTokens > 0 || chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
		events = append(events, types.EventUsage{Usage: types.Usage{
			InputTokens:  chunk.Usage.PromptTokens,
			OutputTokens: chunk.Usage.CompletionTokens,
			TotalTokens:  chunk.Usage.TotalTokens,
		}})
	}

	return events, false, nil
}

func parseSSEReader(reader io.Reader, out *stream.ChannelEventStream) error {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		events, done, err := ParseDataLine(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		if err != nil {
			return err
		}
		for _, event := range events {
			out.Emit(event)
		}
		if done {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func mapStopReason(reason string) types.StopReason {
	switch reason {
	case "stop":
		return types.StopReasonStop
	case "length":
		return types.StopReasonLength
	default:
		return types.StopReasonStop
	}
}
