package gorunner

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/vaayne/anna/agent/runner"
	aistream "github.com/vaayne/anna/pkg/ai/stream"
	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

func TestNewRequiresConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"missing api", Config{Model: "m", APIKey: "k"}},
		{"missing model", Config{API: "anthropic", APIKey: "k"}},
		{"missing api_key", Config{API: "anthropic", Model: "m"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(context.Background(), tt.cfg)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestNewSuccess(t *testing.T) {
	r, err := New(context.Background(), Config{
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
	events := []runner.RPCEvent{
		{Type: "user_message", Summary: "hello"},
		runner.TextDeltaToRPCEvent("Hi "),
		runner.TextDeltaToRPCEvent("there!"),
		{Type: "user_message", Summary: "how are you?"},
		runner.TextDeltaToRPCEvent("I'm fine."),
	}

	msgs := convertHistory(events)

	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}

	// Verify message order and types.
	expected := []string{"user", "assistant", "user", "assistant"}
	for i, msg := range msgs {
		got := messageType(msg)
		if got != expected[i] {
			t.Errorf("message %d: type = %q, want %q", i, got, expected[i])
		}
	}
}

func TestConvertHistoryOnlyAssistant(t *testing.T) {
	events := []runner.RPCEvent{
		runner.TextDeltaToRPCEvent("orphan text"),
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
	events := []runner.RPCEvent{
		{Type: "user_message", Summary: "hi"},
		{Type: "agent_end"},
		{Type: "error", Error: "something"},
		runner.TextDeltaToRPCEvent("reply"),
	}

	msgs := convertHistory(events)

	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
}

func TestConvertHistoryMultipleConsecutiveDeltas(t *testing.T) {
	events := []runner.RPCEvent{
		{Type: "user_message", Summary: "hi"},
		runner.TextDeltaToRPCEvent("a"),
		runner.TextDeltaToRPCEvent("b"),
		runner.TextDeltaToRPCEvent("c"),
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
	events := []runner.RPCEvent{
		{Type: "user_message", Summary: "turn1"},
		runner.TextDeltaToRPCEvent("reply1"),
		{Type: "user_message", Summary: "turn2"},
		runner.TextDeltaToRPCEvent("reply2"),
		{Type: "user_message", Summary: "turn3"},
		runner.TextDeltaToRPCEvent("reply3a"),
		runner.TextDeltaToRPCEvent("reply3b"),
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

	// Last assistant message should have merged deltas.
	am := msgs[5].(aitypes.AssistantMessage)
	tc := am.Content[0].(aitypes.TextContent)
	if tc.Text != "reply3areply3b" {
		t.Errorf("last assistant text = %q, want %q", tc.Text, "reply3areply3b")
	}
}

// fakeProvider implements stream.Provider for testing Chat() without real API calls.
type fakeProvider struct {
	api    string
	events []aitypes.AssistantEvent
	err    error // returned from Stream() if non-nil
}

func (f *fakeProvider) API() string { return f.api }

func (f *fakeProvider) Stream(_ aitypes.Model, _ aitypes.Context, _ aitypes.StreamOptions) (aistream.AssistantEventStream, error) {
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

func (f *fakeProvider) StreamSimple(_ aitypes.Model, _ aitypes.Context, opts aitypes.SimpleStreamOptions) (aistream.AssistantEventStream, error) {
	return f.Stream(aitypes.Model{}, aitypes.Context{}, opts.StreamOptions)
}

// newTestRunner creates a Runner wired to a fake provider.
func newTestRunner(t *testing.T, fp *fakeProvider) *Runner {
	t.Helper()
	r, err := New(context.Background(), Config{
		API:    fp.api,
		Model:  "test-model",
		APIKey: "test-key",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Replace the registry with one containing our fake.
	r.reg.Register(fp)
	return r
}

func TestChatStreamsTextDeltas(t *testing.T) {
	fp := &fakeProvider{
		api: "anthropic",
		events: []aitypes.AssistantEvent{
			aitypes.EventStart{},
			aitypes.EventTextDelta{Text: "Hello "},
			aitypes.EventTextDelta{Text: "world"},
			aitypes.EventStop{Reason: aitypes.StopReasonStop},
		},
	}
	r := newTestRunner(t, fp)

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
	fp := &fakeProvider{
		api: "anthropic",
		events: []aitypes.AssistantEvent{
			aitypes.EventTextDelta{Text: "partial"},
			aitypes.EventError{Err: errors.New("provider boom")},
		},
	}
	r := newTestRunner(t, fp)

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
	if gotErr.Error() != "provider boom" {
		t.Errorf("error = %q, want %q", gotErr.Error(), "provider boom")
	}
}

func TestChatUnknownProvider(t *testing.T) {
	r, err := New(context.Background(), Config{
		API:    "nonexistent",
		Model:  "test-model",
		APIKey: "test-key",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
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
	// When the stream finishes (e.g. provider closes), the Chat channel
	// should close even if context was already cancelled.
	fp := &fakeProvider{
		api: "anthropic",
		events: []aitypes.AssistantEvent{
			aitypes.EventTextDelta{Text: "ok"},
			aitypes.EventStop{Reason: aitypes.StopReasonStop},
		},
	}
	r := newTestRunner(t, fp)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Chat

	ch := r.Chat(ctx, nil, "hi")

	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()

	select {
	case <-done:
		// OK — channel closed without hanging.
	case <-time.After(2 * time.Second):
		t.Fatal("Chat channel did not close after context cancellation")
	}
}

func TestLastActivityUpdatesOnChat(t *testing.T) {
	fp := &fakeProvider{
		api: "anthropic",
		events: []aitypes.AssistantEvent{
			aitypes.EventTextDelta{Text: "ok"},
			aitypes.EventStop{Reason: aitypes.StopReasonStop},
		},
	}
	r := newTestRunner(t, fp)

	before := time.Now()
	time.Sleep(1 * time.Millisecond) // ensure time advances

	ch := r.Chat(context.Background(), nil, "hi")
	for range ch {
	}

	if r.LastActivity().Before(before) {
		t.Errorf("LastActivity %v should be after %v", r.LastActivity(), before)
	}
}

func TestAliveAlwaysTrue(t *testing.T) {
	r, err := New(context.Background(), Config{
		API:    "anthropic",
		Model:  "test-model",
		APIKey: "test-key",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if !r.Alive() {
		t.Error("Alive() should be true before Close")
	}

	r.Close()

	if !r.Alive() {
		t.Error("Alive() should still be true after Close (no subprocess)")
	}
}
