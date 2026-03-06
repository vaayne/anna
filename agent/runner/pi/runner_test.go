package pi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vaayne/anna/agent/runner"
)

// writeMockBinary creates a shell script that acts as a mock Pi process.
// It simply runs "cat" to echo stdin to stdout, ignoring all arguments.
func writeMockBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "mock-pi")
	script := "#!/bin/sh\nexec cat\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestNew(t *testing.T) {
	bin := writeMockBinary(t)
	ctx := context.Background()

	r, err := New(ctx, bin, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	if r.binary != bin {
		t.Errorf("binary = %q, want %q", r.binary, bin)
	}
	if !r.Alive() {
		t.Error("new runner should report Alive()")
	}
}

func TestRunnerChat(t *testing.T) {
	bin := writeMockBinary(t)
	ctx := context.Background()

	r, err := New(ctx, bin, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	// Write a JSON event to stdin (cat echoes it back to stdout).
	evt := runner.RPCEvent{Type: "agent_end"}
	data, _ := json.Marshal(evt)
	data = append(data, '\n')

	r.mu.Lock()
	_, writeErr := r.stdin.Write(data)
	r.mu.Unlock()
	if writeErr != nil {
		t.Fatalf("write to stdin: %v", writeErr)
	}

	select {
	case received := <-r.events:
		if received.Type != "agent_end" {
			t.Errorf("event type = %q, want %q", received.Type, "agent_end")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestRunnerInvalidBinary(t *testing.T) {
	_, err := New(context.Background(), "/nonexistent/binary", "")
	if err == nil {
		t.Fatal("expected error for invalid binary")
	}
}

func TestRunnerClose(t *testing.T) {
	bin := writeMockBinary(t)
	ctx := context.Background()

	r, err := New(ctx, bin, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	if r.Alive() {
		t.Error("runner should not be alive after Close")
	}
}

func TestRunnerChatStreamEvents(t *testing.T) {
	bin := writeMockBinary(t)
	ctx := context.Background()

	r, err := New(ctx, bin, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	// Simulate Pi events by writing to cat's stdin (which echoes to stdout).
	go func() {
		events := []runner.RPCEvent{
			{Type: "message_start"},
			{
				Type:                  "message_update",
				AssistantMessageEvent: json.RawMessage(`{"type":"text_delta","delta":"Hello "}`),
			},
			{
				Type:                  "message_update",
				AssistantMessageEvent: json.RawMessage(`{"type":"text_delta","delta":"world"}`),
			},
			{Type: "agent_end"},
		}

		for _, evt := range events {
			data, _ := json.Marshal(evt)
			data = append(data, '\n')
			r.mu.Lock()
			r.stdin.Write(data)
			r.mu.Unlock()
		}
	}()

	time.Sleep(100 * time.Millisecond)

	stream := r.Chat(ctx, nil, "test")

	var collected string
	for evt := range stream {
		if evt.Err != nil {
			t.Fatalf("unexpected error: %v", evt.Err)
		}
		collected += evt.Text
	}

	if collected != "Hello world" {
		t.Errorf("collected = %q, want %q", collected, "Hello world")
	}
}

func TestRunnerChatErrorEvent(t *testing.T) {
	bin := writeMockBinary(t)
	ctx := context.Background()

	r, err := New(ctx, bin, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	go func() {
		evt := runner.RPCEvent{Type: "error", Error: "something went wrong"}
		data, _ := json.Marshal(evt)
		data = append(data, '\n')
		r.mu.Lock()
		r.stdin.Write(data)
		r.mu.Unlock()
	}()

	time.Sleep(100 * time.Millisecond)
	stream := r.Chat(ctx, nil, "test")

	var gotErr error
	for evt := range stream {
		if evt.Err != nil {
			gotErr = evt.Err
			break
		}
	}

	if gotErr == nil {
		t.Fatal("expected error event")
	}
	if gotErr.Error() != "pi error: something went wrong" {
		t.Errorf("error = %q, want %q", gotErr.Error(), "pi error: something went wrong")
	}
}

func TestRunnerLastActivity(t *testing.T) {
	bin := writeMockBinary(t)
	ctx := context.Background()

	r, err := New(ctx, bin, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	before := r.LastActivity()
	time.Sleep(10 * time.Millisecond)

	go func() {
		time.Sleep(50 * time.Millisecond)
		evt := runner.RPCEvent{Type: "agent_end"}
		data, _ := json.Marshal(evt)
		data = append(data, '\n')
		r.mu.Lock()
		r.stdin.Write(data)
		r.mu.Unlock()
	}()

	_ = r.Chat(ctx, nil, "test")
	after := r.LastActivity()

	if !after.After(before) {
		t.Error("LastActivity should be updated after Chat")
	}
}

func TestRunnerResponseRouting(t *testing.T) {
	bin := writeMockBinary(t)
	ctx := context.Background()

	r, err := New(ctx, bin, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	ch := make(chan *runner.RPCEvent, 1)
	r.pendingMu.Lock()
	r.pending["42"] = ch
	r.pendingMu.Unlock()

	evt := runner.RPCEvent{Type: "response", ID: "42"}
	data, _ := json.Marshal(evt)
	data = append(data, '\n')
	r.mu.Lock()
	r.stdin.Write(data)
	r.mu.Unlock()

	select {
	case resp := <-ch:
		if resp.Type != "response" || resp.ID != "42" {
			t.Errorf("unexpected response: %+v", resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for response")
	}
}

func TestRunnerChatAfterClose(t *testing.T) {
	bin := writeMockBinary(t)
	ctx := context.Background()

	r, err := New(ctx, bin, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.Close()
	time.Sleep(100 * time.Millisecond)

	stream := r.Chat(ctx, nil, "test")
	var gotErr error
	for evt := range stream {
		if evt.Err != nil {
			gotErr = evt.Err
			break
		}
	}
	if gotErr == nil {
		t.Fatal("expected error when chatting with closed runner")
	}
}

func TestRunnerChatContextCancel(t *testing.T) {
	bin := writeMockBinary(t)

	ctx, cancel := context.WithCancel(context.Background())
	r, err := New(ctx, bin, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	// Cancel context after starting to read.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	stream := r.Chat(ctx, nil, "test")
	var gotErr error
	for evt := range stream {
		if evt.Err != nil {
			gotErr = evt.Err
			break
		}
	}
	if gotErr == nil {
		t.Fatal("expected error on context cancel")
	}
}

func TestRPCCommandJSON(t *testing.T) {
	cmd := runner.RPCCommand{ID: "1", Type: "prompt", Message: "hello"}
	data, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded runner.RPCCommand
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.ID != "1" || decoded.Type != "prompt" || decoded.Message != "hello" {
		t.Errorf("decoded = %+v, want ID=1 Type=prompt Message=hello", decoded)
	}
}

func TestRPCCommandOmitEmpty(t *testing.T) {
	cmd := runner.RPCCommand{ID: "1", Type: "abort"}
	data, _ := json.Marshal(cmd)

	var m map[string]interface{}
	json.Unmarshal(data, &m)

	if _, ok := m["message"]; ok {
		t.Error("message field should be omitted when empty")
	}
}

func TestRPCEventUnmarshal(t *testing.T) {
	raw := `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"hi"}}`
	var evt runner.RPCEvent
	if err := json.Unmarshal([]byte(raw), &evt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if evt.Type != "message_update" {
		t.Errorf("type = %q, want message_update", evt.Type)
	}

	var ame runner.AssistantMessageEvent
	if err := json.Unmarshal(evt.AssistantMessageEvent, &ame); err != nil {
		t.Fatalf("unmarshal assistant event: %v", err)
	}
	if ame.Type != "text_delta" || ame.Delta != "hi" {
		t.Errorf("ame = %+v, want text_delta/hi", ame)
	}
}
