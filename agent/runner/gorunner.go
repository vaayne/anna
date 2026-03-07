package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/vaayne/anna/agent/tool"
	"github.com/vaayne/anna/ai/providers/anthropic"
	"github.com/vaayne/anna/ai/providers/openai"
	openairesponse "github.com/vaayne/anna/ai/providers/openai-response"
	"github.com/vaayne/anna/ai/registry"
	aitypes "github.com/vaayne/anna/ai/types"
	"github.com/vaayne/anna/memory"
)

const maxToolIterations = 40

// GoRunnerConfig configures the Go runner.
type GoRunnerConfig struct {
	API         string // provider key: "anthropic", "openai"
	Model       string // e.g. "claude-sonnet-4-20250514"
	APIKey      string
	BaseURL     string        // optional provider base URL override
	WorkDir     string        // working directory for tool execution
	Workspace   string        // workspace dir for skills/memory (e.g. ~/.anna/workspace)
	MemoryStore *memory.Store // persistent memory (soul, user, facts, journal)
	System      string        // optional system prompt override (bypasses BuildSystemPrompt)
	ExtraTools  []tool.Tool   // additional tools to register
}

// GoRunner implements Runner by calling LLM providers directly via Engine.
type GoRunner struct {
	engine *Engine
	reg    *registry.Registry
	tools  *tool.Registry
	model  aitypes.Model
	apiKey string
	system string

	mu           sync.Mutex
	lastActivity time.Time
	log          *slog.Logger
}

// NewGoRunner creates a Go runner with built-in providers.
func NewGoRunner(_ context.Context, cfg GoRunnerConfig) (*GoRunner, error) {
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

	system := cfg.System
	if system == "" {
		if cfg.MemoryStore != nil {
			system = BuildSystemPrompt(cfg.MemoryStore, cfg.Workspace, cfg.WorkDir)
		} else {
			system = defaultBasicPrompt
		}
	}

	tools := tool.NewRegistry(cfg.WorkDir)
	for _, t := range cfg.ExtraTools {
		tools.Register(t)
	}

	return &GoRunner{
		engine:       &Engine{Providers: reg},
		reg:          reg,
		tools:        tools,
		model:        aitypes.Model{API: cfg.API, Name: cfg.Model},
		apiKey:       cfg.APIKey,
		system:       system,
		lastActivity: time.Now(),
		log:          slog.With("component", "go_runner"),
	}, nil
}

// Chat converts history, runs the Engine agent loop, and forwards events to the returned channel.
func (r *GoRunner) Chat(ctx context.Context, history []RPCEvent, message string) <-chan Event {
	out := make(chan Event, 100)

	r.mu.Lock()
	r.lastActivity = time.Now()
	r.mu.Unlock()

	go func() {
		defer close(out)

		messages := convertHistory(history)
		messages = append(messages, aitypes.UserMessage{Content: message})

		cfg := LoopConfig{
			Model:           r.model,
			StreamOptions:   aitypes.StreamOptions{APIKey: r.apiKey},
			MaxTurns:        maxToolIterations,
			Tools:           r.buildToolSet(),
			ToolDefinitions: r.tools.Definitions(),
			System:          r.system,
		}

		if _, err := r.engine.Run(ctx, cfg, messages, func(e loopEvent) {
			for _, evt := range convertLoopEvent(e) {
				out <- evt
			}
		}); err != nil {
			out <- Event{Err: err}
		}
	}()

	return out
}

// Alive always returns true — the Go runner has no subprocess to die.
func (r *GoRunner) Alive() bool { return true }

// LastActivity returns the time of the last Chat call.
func (r *GoRunner) LastActivity() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastActivity
}

// Close is a no-op for the Go runner.
func (r *GoRunner) Close() error { return nil }

// buildToolSet adapts tool.Registry to ToolSet for Engine.
func (r *GoRunner) buildToolSet() ToolSet {
	set := ToolSet{}
	for _, def := range r.tools.Definitions() {
		name := def.Name
		set[name] = func(ctx context.Context, call aitypes.ToolCall) (aitypes.TextContent, error) {
			result, err := r.tools.Execute(ctx, name, call.Arguments)
			return aitypes.TextContent{Text: result}, err
		}
	}
	return set
}

// convertLoopEvent bridges loopEvent to Event(s).
func convertLoopEvent(e loopEvent) []Event {
	switch e := e.(type) {
	case AssistantDelta:
		if d, ok := e.Event.(aitypes.EventTextDelta); ok && d.Text != "" {
			return []Event{{Text: d.Text}}
		}

	case AssistantFinished:
		// Emit Store events for tool calls in the final message.
		var events []Event
		for _, block := range e.Message.Content {
			if call, ok := block.(aitypes.ToolCall); ok {
				rpc := ToolCallToRPCEvent(call)
				events = append(events, Event{Store: &rpc})
			}
		}
		return events

	case ToolStarted:
		return []Event{{ToolUse: &ToolUseEvent{
			Tool:   e.ToolCall.Name,
			Status: "running",
			Input:  summarizeToolInput(e.ToolCall.Name, e.ToolCall.Arguments),
		}}}

	case ToolFinished:
		status := "done"
		detail := ""
		if e.Result.IsError {
			status = "error"
			for _, block := range e.Result.Content {
				if tc, ok := block.(aitypes.TextContent); ok {
					detail = tc.Text
				}
			}
		}
		rpc := ToolResultToRPCEvent(e.Result)
		return []Event{
			{ToolUse: &ToolUseEvent{
				Tool:   e.Result.ToolName,
				Status: status,
				Input:  summarizeToolInput(e.Result.ToolName, nil),
				Detail: detail,
			}},
			{Store: &rpc},
		}

	case AgentErrored:
		return []Event{{Err: e.Err}}
	}

	return nil
}

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
func convertHistory(events []RPCEvent) []aitypes.Message {
	var messages []aitypes.Message
	var textBuf string
	var pendingCalls []aitypes.ToolCall
	seenCallIDs := map[string]bool{}

	flush := func() {
		if textBuf != "" {
			messages = append(messages, aitypes.AssistantMessage{
				Content: []aitypes.ContentBlock{aitypes.TextContent{Text: textBuf}},
			})
			textBuf = ""
		}
	}

	flushToolCalls := func() {
		if len(pendingCalls) > 0 {
			blocks := make([]aitypes.ContentBlock, 0, len(pendingCalls)+1)
			if textBuf != "" {
				blocks = append(blocks, aitypes.TextContent{Text: textBuf})
				textBuf = ""
			}
			for _, c := range pendingCalls {
				blocks = append(blocks, c)
			}
			messages = append(messages, aitypes.AssistantMessage{Content: blocks})
			pendingCalls = nil
		}
	}

	for _, evt := range events {
		switch evt.Type {
		case RPCEventUserMessage:
			flushToolCalls()
			flush()
			messages = append(messages, aitypes.UserMessage{Content: evt.Summary})

		case RPCEventMessageUpdate:
			if evt.Summary != "" {
				textBuf += evt.Summary
			} else if len(evt.AssistantMessageEvent) > 0 {
				var ame AssistantMessageEvent
				if json.Unmarshal(evt.AssistantMessageEvent, &ame) == nil && ame.Type == "text_delta" {
					textBuf += ame.Delta
				}
			}

		case RPCEventToolCall:
			var args map[string]any
			_ = json.Unmarshal(evt.Result, &args)
			seenCallIDs[evt.ID] = true
			pendingCalls = append(pendingCalls, aitypes.ToolCall{
				ID:        evt.ID,
				Name:      evt.Tool,
				Arguments: args,
			})

		case RPCEventToolResult:
			// Skip orphaned tool results (no matching tool call).
			if !seenCallIDs[evt.ID] {
				continue
			}
			flushToolCalls()
			var content string
			_ = json.Unmarshal(evt.Result, &content)
			messages = append(messages, aitypes.ToolResultMessage{
				ToolCallID: evt.ID,
				ToolName:   evt.Tool,
				Content:    []aitypes.ContentBlock{aitypes.TextContent{Text: content}},
				IsError:    evt.Error != "",
			})
		}
	}

	flushToolCalls()
	flush()
	return messages
}
