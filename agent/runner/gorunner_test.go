package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	aistream "github.com/vaayne/anna/ai/stream"
	aitypes "github.com/vaayne/anna/ai/types"
)

func TestNewGoRunnerRequiresConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  GoRunnerConfig
	}{
		{"missing api", GoRunnerConfig{Model: "m", APIKey: "k"}},
		{"missing model", GoRunnerConfig{API: "anthropic", APIKey: "k"}},
		{"missing api_key", GoRunnerConfig{API: "anthropic", Model: "m"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewGoRunner(context.Background(), tt.cfg)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestNewGoRunnerSuccess(t *testing.T) {
	r, err := NewGoRunner(context.Background(), GoRunnerConfig{
		API:    "anthropic",
		Model:  "claude-sonnet-4-20250514",
		APIKey: "test-key",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.Alive() {
		t.Error("new runner should be alive")
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestConvertHistoryEmpty(t *testing.T) {
	msgs := convertHistory(nil)
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages, got %d", len(msgs))
	}
}

func TestConvertHistoryRoundTrip(t *testing.T) {
	events := []RPCEvent{
		{Type: RPCEventUserMessage, Summary: "hello"},
		TextDeltaToRPCEvent("Hi "),
		TextDeltaToRPCEvent("there!"),
		{Type: RPCEventUserMessage, Summary: "how are you?"},
		TextDeltaToRPCEvent("I'm fine."),
	}

	msgs := convertHistory(events)

	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}

	expected := []string{"user", "assistant", "user", "assistant"}
	for i, msg := range msgs {
		got := messageType(msg)
		if got != expected[i] {
			t.Errorf("message %d: type = %q, want %q", i, got, expected[i])
		}
	}
}

func TestConvertHistoryOnlyAssistant(t *testing.T) {
	events := []RPCEvent{
		TextDeltaToRPCEvent("orphan text"),
	}

	msgs := convertHistory(events)

	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if messageType(msgs[0]) != "assistant" {
		t.Errorf("expected assistant, got %q", messageType(msgs[0]))
	}
}

func messageType(msg aitypes.Message) string {
	switch msg.(type) {
	case aitypes.UserMessage:
		return "user"
	case aitypes.AssistantMessage:
		return "assistant"
	case aitypes.ToolResultMessage:
		return "tool"
	default:
		return fmt.Sprintf("unknown(%T)", msg)
	}
}

func TestConvertHistorySkipsUnknownTypes(t *testing.T) {
	events := []RPCEvent{
		{Type: RPCEventUserMessage, Summary: "hi"},
		{Type: RPCEventAgentEnd},
		{Type: "error", Error: "something"},
		TextDeltaToRPCEvent("reply"),
	}

	msgs := convertHistory(events)

	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
}

func TestConvertHistoryMultipleConsecutiveDeltas(t *testing.T) {
	events := []RPCEvent{
		{Type: RPCEventUserMessage, Summary: "hi"},
		TextDeltaToRPCEvent("a"),
		TextDeltaToRPCEvent("b"),
		TextDeltaToRPCEvent("c"),
	}

	msgs := convertHistory(events)

	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (1 user + 1 merged assistant), got %d", len(msgs))
	}
	if messageType(msgs[1]) != "assistant" {
		t.Fatalf("expected assistant, got %q", messageType(msgs[1]))
	}
	am := msgs[1].(aitypes.AssistantMessage)
	if len(am.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(am.Content))
	}
	tc, ok := am.Content[0].(aitypes.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", am.Content[0])
	}
	if tc.Text != "abc" {
		t.Errorf("merged text = %q, want %q", tc.Text, "abc")
	}
}

func TestConvertHistoryAlternatingTurns(t *testing.T) {
	events := []RPCEvent{
		{Type: RPCEventUserMessage, Summary: "turn1"},
		TextDeltaToRPCEvent("reply1"),
		{Type: RPCEventUserMessage, Summary: "turn2"},
		TextDeltaToRPCEvent("reply2"),
		{Type: RPCEventUserMessage, Summary: "turn3"},
		TextDeltaToRPCEvent("reply3a"),
		TextDeltaToRPCEvent("reply3b"),
	}

	msgs := convertHistory(events)

	if len(msgs) != 6 {
		t.Fatalf("expected 6 messages, got %d", len(msgs))
	}

	expected := []string{"user", "assistant", "user", "assistant", "user", "assistant"}
	for i, msg := range msgs {
		got := messageType(msg)
		if got != expected[i] {
			t.Errorf("message %d: type = %q, want %q", i, got, expected[i])
		}
	}

	am := msgs[5].(aitypes.AssistantMessage)
	tc := am.Content[0].(aitypes.TextContent)
	if tc.Text != "reply3areply3b" {
		t.Errorf("last assistant text = %q, want %q", tc.Text, "reply3areply3b")
	}
}

func TestConvertHistoryWithToolEvents(t *testing.T) {
	argsJSON, _ := json.Marshal(map[string]any{"command": "echo hello"})
	contentJSON, _ := json.Marshal("hello\n")

	events := []RPCEvent{
		{Type: RPCEventUserMessage, Summary: "run echo"},
		TextDeltaToRPCEvent("Let me run that."),
		{Type: RPCEventToolCall, ID: "tc_1", Tool: "bash", Result: argsJSON},
		{Type: RPCEventToolResult, ID: "tc_1", Tool: "bash", Result: contentJSON},
		TextDeltaToRPCEvent("Done!"),
	}

	msgs := convertHistory(events)

	// Expected: user, assistant(text+toolcall), tool_result, assistant(text)
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}

	expectedTypes := []string{"user", "assistant", "tool", "assistant"}
	for i, msg := range msgs {
		got := messageType(msg)
		if got != expectedTypes[i] {
			t.Errorf("message %d: type = %q, want %q", i, got, expectedTypes[i])
		}
	}

	// Assistant message should have text + tool call.
	am := msgs[1].(aitypes.AssistantMessage)
	if len(am.Content) != 2 {
		t.Fatalf("expected 2 content blocks in assistant message, got %d", len(am.Content))
	}
	if _, ok := am.Content[0].(aitypes.TextContent); !ok {
		t.Errorf("expected TextContent first, got %T", am.Content[0])
	}
	if tc, ok := am.Content[1].(aitypes.ToolCall); !ok {
		t.Errorf("expected ToolCall second, got %T", am.Content[1])
	} else if tc.Name != "bash" {
		t.Errorf("tool call name = %q, want %q", tc.Name, "bash")
	}

	// Tool result.
	tr := msgs[2].(aitypes.ToolResultMessage)
	if tr.ToolCallID != "tc_1" {
		t.Errorf("tool result ID = %q, want %q", tr.ToolCallID, "tc_1")
	}
}

func TestConvertHistoryOrphanedToolResult(t *testing.T) {
	contentJSON, _ := json.Marshal("some result")

	events := []RPCEvent{
		{Type: RPCEventUserMessage, Summary: "hello"},
		// Orphaned tool result — no matching tool call.
		{Type: RPCEventToolResult, ID: "orphan_1", Tool: "bash", Result: contentJSON},
		TextDeltaToRPCEvent("Hi there!"),
	}

	msgs := convertHistory(events)

	// Expected: user, assistant(text). Orphaned tool result should be skipped.
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if messageType(msgs[0]) != "user" {
		t.Errorf("message 0: type = %q, want %q", messageType(msgs[0]), "user")
	}
	if messageType(msgs[1]) != "assistant" {
		t.Errorf("message 1: type = %q, want %q", messageType(msgs[1]), "assistant")
	}
}

// goRunnerFakeProvider implements stream.Provider for testing Chat() without real API calls.
type goRunnerFakeProvider struct {
	api    string
	events []aitypes.AssistantEvent
	err    error
}

func (f *goRunnerFakeProvider) API() string { return f.api }

func (f *goRunnerFakeProvider) Stream(_ aitypes.Model, _ aitypes.Context, _ aitypes.StreamOptions) (aistream.AssistantEventStream, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := aistream.NewChannelEventStream(len(f.events) + 1)
	go func() {
		for _, evt := range f.events {
			out.Emit(evt)
		}
		out.Finish(nil)
	}()
	return out, nil
}

func (f *goRunnerFakeProvider) StreamSimple(_ aitypes.Model, _ aitypes.Context, opts aitypes.SimpleStreamOptions) (aistream.AssistantEventStream, error) {
	return f.Stream(aitypes.Model{}, aitypes.Context{}, opts.StreamOptions)
}

// newTestGoRunner creates a GoRunner wired to a fake provider.
func newTestGoRunner(t *testing.T, fp *goRunnerFakeProvider) *GoRunner {
	t.Helper()
	r, err := NewGoRunner(context.Background(), GoRunnerConfig{
		API:    fp.api,
		Model:  "test-model",
		APIKey: "test-key",
	})
	if err != nil {
		t.Fatalf("NewGoRunner: %v", err)
	}
	r.reg.Register(fp)
	return r
}

func TestChatStreamsTextDeltas(t *testing.T) {
	fp := &goRunnerFakeProvider{
		api: "anthropic",
		events: []aitypes.AssistantEvent{
			aitypes.EventStart{},
			aitypes.EventTextDelta{Text: "Hello "},
			aitypes.EventTextDelta{Text: "world"},
			aitypes.EventStop{Reason: aitypes.StopReasonStop},
		},
	}
	r := newTestGoRunner(t, fp)

	ch := r.Chat(context.Background(), nil, "hi")

	var collected string
	for evt := range ch {
		if evt.Err != nil {
			t.Fatalf("unexpected error: %v", evt.Err)
		}
		collected += evt.Text
	}

	if collected != "Hello world" {
		t.Errorf("collected = %q, want %q", collected, "Hello world")
	}
}

func TestChatStreamError(t *testing.T) {
	fp := &goRunnerFakeProvider{
		api: "anthropic",
		err: errors.New("provider boom"),
	}
	r := newTestGoRunner(t, fp)

	ch := r.Chat(context.Background(), nil, "hi")

	var gotErr error
	for evt := range ch {
		if evt.Err != nil {
			gotErr = evt.Err
			break
		}
	}

	if gotErr == nil {
		t.Fatal("expected error from stream")
	}
}

func TestChatUnknownProvider(t *testing.T) {
	r, err := NewGoRunner(context.Background(), GoRunnerConfig{
		API:    "nonexistent",
		Model:  "test-model",
		APIKey: "test-key",
	})
	if err != nil {
		t.Fatalf("NewGoRunner: %v", err)
	}

	ch := r.Chat(context.Background(), nil, "hi")

	var gotErr error
	for evt := range ch {
		if evt.Err != nil {
			gotErr = evt.Err
			break
		}
	}

	if gotErr == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestChatContextCancellation(t *testing.T) {
	fp := &goRunnerFakeProvider{
		api: "anthropic",
		events: []aitypes.AssistantEvent{
			aitypes.EventTextDelta{Text: "ok"},
			aitypes.EventStop{Reason: aitypes.StopReasonStop},
		},
	}
	r := newTestGoRunner(t, fp)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ch := r.Chat(ctx, nil, "hi")

	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Chat channel did not close after context cancellation")
	}
}

func TestLastActivityUpdatesOnChat(t *testing.T) {
	fp := &goRunnerFakeProvider{
		api: "anthropic",
		events: []aitypes.AssistantEvent{
			aitypes.EventTextDelta{Text: "ok"},
			aitypes.EventStop{Reason: aitypes.StopReasonStop},
		},
	}
	r := newTestGoRunner(t, fp)

	before := time.Now()
	time.Sleep(1 * time.Millisecond)

	ch := r.Chat(context.Background(), nil, "hi")
	for range ch {
	}

	if r.LastActivity().Before(before) {
		t.Errorf("LastActivity %v should be after %v", r.LastActivity(), before)
	}
}

// sequentialFakeProvider returns different event sequences on successive Stream calls.
type sequentialFakeProvider struct {
	api    string
	rounds [][]aitypes.AssistantEvent
	call   int
	mu     sync.Mutex
}

func (f *sequentialFakeProvider) API() string { return f.api }

func (f *sequentialFakeProvider) Stream(_ aitypes.Model, _ aitypes.Context, _ aitypes.StreamOptions) (aistream.AssistantEventStream, error) {
	f.mu.Lock()
	idx := f.call
	f.call++
	f.mu.Unlock()

	events := f.rounds[idx]
	out := aistream.NewChannelEventStream(len(events) + 1)
	go func() {
		for _, evt := range events {
			out.Emit(evt)
		}
		out.Finish(nil)
	}()
	return out, nil
}

func (f *sequentialFakeProvider) StreamSimple(_ aitypes.Model, _ aitypes.Context, opts aitypes.SimpleStreamOptions) (aistream.AssistantEventStream, error) {
	return f.Stream(aitypes.Model{}, aitypes.Context{}, opts.StreamOptions)
}

func TestChatToolUseLoop(t *testing.T) {
	dir := t.TempDir()
	fp := &sequentialFakeProvider{
		api: "anthropic",
		rounds: [][]aitypes.AssistantEvent{
			{
				aitypes.EventToolCallDelta{ID: "tc_1", Name: "bash"},
				aitypes.EventToolCallDelta{ID: "tc_1", Arguments: `{"command": "echo hello"}`},
				aitypes.EventStop{Reason: aitypes.StopReasonToolUse},
			},
			{
				aitypes.EventTextDelta{Text: "The result is hello"},
				aitypes.EventStop{Reason: aitypes.StopReasonStop},
			},
		},
	}

	r, err := NewGoRunner(context.Background(), GoRunnerConfig{
		API:     fp.api,
		Model:   "test-model",
		APIKey:  "test-key",
		WorkDir: dir,
	})
	if err != nil {
		t.Fatalf("NewGoRunner: %v", err)
	}
	r.reg.Register(fp)

	ch := r.Chat(context.Background(), nil, "run echo hello")

	var collected string
	for evt := range ch {
		if evt.Err != nil {
			t.Fatalf("unexpected error: %v", evt.Err)
		}
		collected += evt.Text
	}

	if collected != "The result is hello" {
		t.Errorf("collected = %q, want %q", collected, "The result is hello")
	}
}

func TestAliveAlwaysTrue(t *testing.T) {
	r, err := NewGoRunner(context.Background(), GoRunnerConfig{
		API:    "anthropic",
		Model:  "test-model",
		APIKey: "test-key",
	})
	if err != nil {
		t.Fatalf("NewGoRunner: %v", err)
	}

	if !r.Alive() {
		t.Error("Alive() should be true before Close")
	}

	_ = r.Close()

	if !r.Alive() {
		t.Error("Alive() should still be true after Close (no subprocess)")
	}
}
