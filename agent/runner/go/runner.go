package gorunner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/vaayne/anna/agent/runner"
	"github.com/vaayne/anna/agent/runner/go/tool"
	"github.com/vaayne/anna/pkg/ai/providers/anthropic"
	"github.com/vaayne/anna/pkg/ai/providers/openai"
	openairesponse "github.com/vaayne/anna/pkg/ai/providers/openai-response"
	"github.com/vaayne/anna/pkg/ai/registry"
	"github.com/vaayne/anna/pkg/ai/stream"
	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

const maxToolIterations = 40

// Config configures the Go runner.
type Config struct {
	API     string // provider key: "anthropic", "openai"
	Model   string // e.g. "claude-sonnet-4-20250514"
	APIKey  string
	System  string // system prompt
	BaseURL string // optional provider base URL override
	WorkDir string // working directory for tool execution
}

// Runner implements runner.Runner by calling LLM providers directly.
type Runner struct {
	reg    *registry.Registry
	tools  *tool.Registry
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
	reg.Register(openairesponse.New(openairesponse.Config{BaseURL: cfg.BaseURL}))

	return &Runner{
		reg:          reg,
		tools:        tool.NewRegistry(cfg.WorkDir),
		model:        aitypes.Model{API: cfg.API, Name: cfg.Model},
		apiKey:       cfg.APIKey,
		system:       cfg.System,
		lastActivity: time.Now(),
		log:          slog.With("component", "go_runner"),
	}, nil
}

// toolCallAccumulator collects streamed tool call deltas into complete calls.
type toolCallAccumulator struct {
	calls []aitypes.ToolCall
	args  map[string]string // id → accumulated argument JSON
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{args: make(map[string]string)}
}

func (a *toolCallAccumulator) addDelta(d aitypes.EventToolCallDelta) {
	if d.ID != "" && d.Name != "" {
		a.calls = append(a.calls, aitypes.ToolCall{ID: d.ID, Name: d.Name})
		a.args[d.ID] = ""
	}
	if d.Arguments != "" {
		id := d.ID
		if id == "" && len(a.calls) > 0 {
			id = a.calls[len(a.calls)-1].ID
		}
		a.args[id] += d.Arguments
	}
}

func (a *toolCallAccumulator) finalize() []aitypes.ToolCall {
	for i := range a.calls {
		raw := a.args[a.calls[i].ID]
		if raw != "" {
			var parsed map[string]any
			if json.Unmarshal([]byte(raw), &parsed) == nil {
				a.calls[i].Arguments = parsed
			}
		}
	}
	return a.calls
}

// Chat converts history, streams from the LLM provider, executes tool calls
// in a loop, and forwards text deltas to the returned channel.
func (r *Runner) Chat(ctx context.Context, history []runner.RPCEvent, message string) <-chan runner.Event {
	out := make(chan runner.Event, 100)

	r.mu.Lock()
	r.lastActivity = time.Now()
	r.mu.Unlock()

	go func() {
		defer close(out)

		messages := convertHistory(history)
		messages = append(messages, aitypes.UserMessage{Content: message})

		for i := range maxToolIterations {
			toolCalls, err := r.streamOnce(ctx, messages, out)
			if err != nil {
				out <- runner.Event{Err: err}
				return
			}

			if len(toolCalls) == 0 {
				return
			}

			r.log.Debug("executing tool calls", "iteration", i, "count", len(toolCalls))

			// Build assistant message with tool calls.
			assistantBlocks := make([]aitypes.ContentBlock, 0, len(toolCalls))
			for _, tc := range toolCalls {
				assistantBlocks = append(assistantBlocks, tc)
			}
			messages = append(messages, aitypes.AssistantMessage{Content: assistantBlocks})

			// Execute each tool and append results.
			for _, tc := range toolCalls {
				out <- runner.Event{ToolUse: &runner.ToolUseEvent{
					Tool:   tc.Name,
					Status: "running",
					Input:  summarizeToolInput(tc.Name, tc.Arguments),
				}}

				result, execErr := r.tools.Execute(ctx, tc.Name, tc.Arguments)
				isError := execErr != nil
				content := result
				if isError {
					content = execErr.Error()
					if result != "" {
						content = result + "\n" + content
					}
				}

				status := "done"
				detail := ""
				if isError {
					status = "error"
					detail = execErr.Error()
				}
				out <- runner.Event{ToolUse: &runner.ToolUseEvent{
					Tool:   tc.Name,
					Status: status,
					Input:  summarizeToolInput(tc.Name, tc.Arguments),
					Detail: detail,
				}}

				r.log.Debug("tool result", "tool", tc.Name, "is_error", isError, "result_len", len(content))

				messages = append(messages, aitypes.ToolResultMessage{
					ToolCallID: tc.ID,
					ToolName:   tc.Name,
					Content:    []aitypes.ContentBlock{aitypes.TextContent{Text: content}},
					IsError:    isError,
				})
			}
		}

		out <- runner.Event{Err: fmt.Errorf("max tool iterations (%d) reached", maxToolIterations)}
	}()

	return out
}

// streamOnce runs a single LLM streaming request, forwarding text deltas to
// out and returning any accumulated tool calls.
func (r *Runner) streamOnce(ctx context.Context, messages []aitypes.Message, out chan<- runner.Event) ([]aitypes.ToolCall, error) {
	opts := aitypes.StreamOptions{APIKey: r.apiKey}
	aiCtx := aitypes.Context{
		System:   r.system,
		Messages: messages,
		Tools:    r.tools.Definitions(),
	}

	eventStream, err := stream.Stream(r.model, aiCtx, opts, r.reg)
	if err != nil {
		return nil, fmt.Errorf("stream: %w", err)
	}

	acc := newToolCallAccumulator()

	for event := range eventStream.Events() {
		switch e := event.(type) {
		case aitypes.EventTextDelta:
			if e.Text != "" {
				out <- runner.Event{Text: e.Text}
			}
		case aitypes.EventToolCallDelta:
			acc.addDelta(e)
		case aitypes.EventError:
			if e.Err != nil {
				return nil, e.Err
			}
		}
	}

	if err := eventStream.Wait(); err != nil {
		return nil, err
	}

	return acc.finalize(), nil
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

// summarizeToolInput returns a short human-readable summary of tool arguments.
func summarizeToolInput(toolName string, args map[string]any) string {
	switch toolName {
	case "bash":
		if cmd, ok := args["command"].(string); ok {
			if len(cmd) > 80 {
				return cmd[:80] + "..."
			}
			return cmd
		}
	case "read":
		if fp, ok := args["file_path"].(string); ok {
			return fp
		}
	case "write":
		if fp, ok := args["file_path"].(string); ok {
			return fp
		}
	case "edit":
		if fp, ok := args["file_path"].(string); ok {
			return fp
		}
	}
	return ""
}

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
