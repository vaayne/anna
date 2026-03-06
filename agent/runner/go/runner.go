package gorunner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/vaayne/anna/agent/runner"
	"github.com/vaayne/anna/pkg/ai/providers/anthropic"
	"github.com/vaayne/anna/pkg/ai/providers/openai"
	"github.com/vaayne/anna/pkg/ai/registry"
	"github.com/vaayne/anna/pkg/ai/stream"
	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

// Config configures the Go runner.
type Config struct {
	API     string // provider key: "anthropic", "openai"
	Model   string // e.g. "claude-sonnet-4-20250514"
	APIKey  string
	System  string // system prompt
	BaseURL string // optional provider base URL override
}

// Runner implements runner.Runner by calling LLM providers directly.
type Runner struct {
	reg    *registry.Registry
	model  aitypes.Model
	apiKey string
	system string

	mu           sync.Mutex
	lastActivity time.Time
	log          *slog.Logger
}

// New creates a Go runner with built-in providers.
func New(_ context.Context, cfg Config) (*Runner, error) {
	if cfg.API == "" {
		return nil, fmt.Errorf("go runner: api is required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("go runner: model is required")
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("go runner: api_key is required")
	}

	reg := registry.New()
	reg.Register(anthropic.New(anthropic.Config{BaseURL: cfg.BaseURL}))
	reg.Register(openai.New(openai.Config{BaseURL: cfg.BaseURL}))

	return &Runner{
		reg:          reg,
		model:        aitypes.Model{API: cfg.API, Name: cfg.Model},
		apiKey:       cfg.APIKey,
		system:       cfg.System,
		lastActivity: time.Now(),
		log:          slog.With("component", "go_runner"),
	}, nil
}

// Chat converts history, streams from the LLM provider, and forwards text
// deltas to the returned channel.
func (r *Runner) Chat(ctx context.Context, history []runner.RPCEvent, message string) <-chan runner.Event {
	out := make(chan runner.Event, 100)

	r.mu.Lock()
	r.lastActivity = time.Now()
	r.mu.Unlock()

	go func() {
		defer close(out)

		messages := convertHistory(history)
		messages = append(messages, aitypes.UserMessage{Content: message})

		opts := aitypes.StreamOptions{APIKey: r.apiKey}
		aiCtx := aitypes.Context{
			System:   r.system,
			Messages: messages,
		}

		eventStream, err := stream.Stream(r.model, aiCtx, opts, r.reg)
		if err != nil {
			out <- runner.Event{Err: fmt.Errorf("stream: %w", err)}
			return
		}

		for event := range eventStream.Events() {
			switch e := event.(type) {
			case aitypes.EventTextDelta:
				if e.Text != "" {
					out <- runner.Event{Text: e.Text}
				}
			case aitypes.EventError:
				if e.Err != nil {
					out <- runner.Event{Err: e.Err}
					return
				}
			}
		}

		if err := eventStream.Wait(); err != nil {
			out <- runner.Event{Err: err}
		}
	}()

	return out
}

// Alive always returns true — the Go runner has no subprocess to die.
func (r *Runner) Alive() bool { return true }

// LastActivity returns the time of the last Chat call.
func (r *Runner) LastActivity() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastActivity
}

// Close is a no-op for the Go runner.
func (r *Runner) Close() error { return nil }

// convertHistory rebuilds []aitypes.Message from RPCEvent history.
// User messages (type "user_message") become UserMessage.
// Consecutive text_delta events are merged into a single AssistantMessage.
func convertHistory(events []runner.RPCEvent) []aitypes.Message {
	var messages []aitypes.Message
	var textBuf string

	flush := func() {
		if textBuf != "" {
			messages = append(messages, aitypes.AssistantMessage{
				Content: []aitypes.ContentBlock{aitypes.TextContent{Text: textBuf}},
			})
			textBuf = ""
		}
	}

	for _, evt := range events {
		switch evt.Type {
		case "user_message":
			flush()
			messages = append(messages, aitypes.UserMessage{Content: evt.Summary})

		case "message_update":
			if len(evt.AssistantMessageEvent) > 0 {
				var ame runner.AssistantMessageEvent
				if json.Unmarshal(evt.AssistantMessageEvent, &ame) == nil && ame.Type == "text_delta" {
					textBuf += ame.Delta
				}
			}
		}
	}

	flush()
	return messages
}
