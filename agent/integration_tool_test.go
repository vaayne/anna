package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vaayne/anna/pkg/agent/core"
	agenttypes "github.com/vaayne/anna/pkg/agent/types"
	"github.com/vaayne/anna/pkg/ai/providers/anthropic"
	"github.com/vaayne/anna/pkg/ai/providers/openai"
	openairesponse "github.com/vaayne/anna/pkg/ai/providers/openai-response"
	"github.com/vaayne/anna/pkg/ai/registry"
	"github.com/vaayne/anna/pkg/ai/stream"
	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

func skipWithoutAPIKey(t *testing.T) string {
	t.Helper()
	key := os.Getenv("ANNA_API_KEY")
	if key == "" {
		t.Skip("ANNA_API_KEY not set, skipping integration test")
	}
	return key
}

func TestIntegrationToolUseAllProviders(t *testing.T) {
	apiKey := skipWithoutAPIKey(t)
	baseURL := os.Getenv("ANNA_BASE_URL")
	if baseURL == "" {
		baseURL = "https://cc2.vaayne.com"
	}

	model := os.Getenv("ANNA_TEST_MODEL")
	if model == "" {
		model = "gpt-5.4"
	}

	providers := []struct {
		name    string
		baseURL string
		factory func(cfg struct{ BaseURL string }) stream.Provider
	}{
		{
			name:    "anthropic",
			baseURL: baseURL,
			factory: func(cfg struct{ BaseURL string }) stream.Provider {
				return anthropic.New(anthropic.Config{BaseURL: cfg.BaseURL})
			},
		},
		{
			name:    "openai",
			baseURL: baseURL + "/v1",
			factory: func(cfg struct{ BaseURL string }) stream.Provider {
				return openai.New(openai.Config{BaseURL: cfg.BaseURL})
			},
		},
		{
			name:    "openai-response",
			baseURL: baseURL + "/v1",
			factory: func(cfg struct{ BaseURL string }) stream.Provider {
				return openairesponse.New(openairesponse.Config{BaseURL: cfg.BaseURL})
			},
		},
	}

	toolDef := aitypes.ToolDefinition{
		Name:        "get_weather",
		Description: "Get the current weather for a city. Always call this tool when asked about weather.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city": map[string]any{
					"type":        "string",
					"description": "The city name to get weather for.",
				},
			},
			"required": []string{"city"},
		},
	}

	for _, p := range providers {
		t.Run(p.name, func(t *testing.T) {
			reg := registry.New()
			reg.Register(p.factory(struct{ BaseURL string }{BaseURL: p.baseURL}))

			engine := &core.Engine{Providers: reg}

			var toolCalled atomic.Bool
			var capturedCity string

			tools := agenttypes.ToolSet{
				"get_weather": func(ctx context.Context, call aitypes.ToolCall) (aitypes.TextContent, error) {
					toolCalled.Store(true)
					city, _ := call.Arguments["city"].(string)
					capturedCity = city
					return aitypes.TextContent{Text: fmt.Sprintf("Weather in %s: 22°C, sunny", city)}, nil
				},
			}

			cfg := agenttypes.Config{
				Model:           aitypes.Model{API: p.name, Name: model},
				StreamOptions:   aitypes.StreamOptions{APIKey: apiKey},
				MaxTurns:        5,
				Tools:           tools,
				ToolDefinitions: []aitypes.ToolDefinition{toolDef},
				System:          "You are a helpful assistant. When asked about weather, always use the get_weather tool. Be concise.",
			}

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			messages := []aitypes.Message{
				aitypes.UserMessage{Content: "What's the weather in Tokyo?"},
			}

			var events []agenttypes.Event
			history, err := engine.Run(ctx, cfg, messages, func(e agenttypes.Event) {
				events = append(events, e)
			})
			if err != nil {
				t.Fatalf("engine.Run error: %v", err)
			}

			// Verify tool was called.
			if !toolCalled.Load() {
				t.Fatal("get_weather tool was never called")
			}

			// Verify arguments were parsed correctly.
			if capturedCity == "" {
				t.Fatal("tool was called but city argument was empty — argument accumulation likely broken")
			}
			if !strings.Contains(strings.ToLower(capturedCity), "tokyo") {
				t.Errorf("expected city to contain 'tokyo', got %q", capturedCity)
			}

			// Verify history has the expected shape: user, assistant(tool_call), tool_result, assistant(text).
			if len(history) < 4 {
				t.Errorf("expected at least 4 messages in history, got %d", len(history))
			}

			// Verify final assistant message has text content.
			lastMsg := history[len(history)-1]
			assistantMsg, ok := lastMsg.(aitypes.AssistantMessage)
			if !ok {
				t.Fatalf("expected last message to be AssistantMessage, got %T", lastMsg)
			}
			var finalText string
			for _, block := range assistantMsg.Content {
				if tc, ok := block.(aitypes.TextContent); ok {
					finalText += tc.Text
				}
			}
			if finalText == "" {
				t.Error("final assistant message has no text content")
			}

			// Log for debugging.
			t.Logf("provider=%s city=%q final_text=%q history_len=%d events=%d",
				p.name, capturedCity, truncate(finalText, 100), len(history), len(events))
		})
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
