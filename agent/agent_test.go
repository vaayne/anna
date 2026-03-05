package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestNewAgent(t *testing.T) {
	ag := NewAgent("/usr/bin/echo", "/tmp/session")
	if ag.binary != "/usr/bin/echo" {
		t.Errorf("binary = %q, want %q", ag.binary, "/usr/bin/echo")
	}
	if ag.sessionPath != "/tmp/session" {
		t.Errorf("sessionPath = %q, want %q", ag.sessionPath, "/tmp/session")
	}
	if !ag.Alive() {
		t.Error("new agent should report Alive()")
	}
}

func TestAgentStartWithMock(t *testing.T) {
	bin := writeMockBinary(t)
	ag := NewAgent(bin, t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ag.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ag.Stop()

	if !ag.Alive() {
		t.Error("agent should be alive after start")
	}

	// Write a JSON event to stdin (cat echoes it back to stdout).
	evt := RPCEvent{Type: "agent_end"}
	data, _ := json.Marshal(evt)
	data = append(data, '\n')

	ag.mu.Lock()
	_, err := ag.stdin.Write(data)
	ag.mu.Unlock()
	if err != nil {
		t.Fatalf("write to stdin: %v", err)
	}

	select {
	case received := <-ag.events:
		if received.Type != "agent_end" {
			t.Errorf("event type = %q, want %q", received.Type, "agent_end")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestAgentStartInvalidBinary(t *testing.T) {
	ag := NewAgent("/nonexistent/binary", t.TempDir())
	err := ag.Start(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid binary")
	}
}

func TestAgentStop(t *testing.T) {
	bin := writeMockBinary(t)
	ag := NewAgent(bin, t.TempDir())
	ctx := context.Background()

	if err := ag.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := ag.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	if ag.Alive() {
		t.Error("agent should not be alive after stop")
	}
}

func TestAgentSendPromptStreamEvents(t *testing.T) {
	bin := writeMockBinary(t)
	ag := NewAgent(bin, t.TempDir())
	ctx := context.Background()

	if err := ag.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ag.Stop()

	// Simulate Pi events by writing to cat's stdin (which echoes to stdout).
	go func() {
		events := []RPCEvent{
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
			ag.mu.Lock()
			ag.stdin.Write(data)
			ag.mu.Unlock()
		}
	}()

	time.Sleep(100 * time.Millisecond)

	stream := ag.SendPrompt(ctx, "test")

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

func TestAgentSendPromptErrorEvent(t *testing.T) {
	bin := writeMockBinary(t)
	ag := NewAgent(bin, t.TempDir())
	ctx := context.Background()

	if err := ag.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ag.Stop()

	go func() {
		evt := RPCEvent{Type: "error", Error: "something went wrong"}
		data, _ := json.Marshal(evt)
		data = append(data, '\n')
		ag.mu.Lock()
		ag.stdin.Write(data)
		ag.mu.Unlock()
	}()

	time.Sleep(100 * time.Millisecond)
	stream := ag.SendPrompt(ctx, "test")

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

func TestAgentLastActivity(t *testing.T) {
	bin := writeMockBinary(t)
	ag := NewAgent(bin, t.TempDir())
	before := ag.LastActivity()

	time.Sleep(10 * time.Millisecond)

	ctx := context.Background()
	if err := ag.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ag.Stop()

	go func() {
		time.Sleep(50 * time.Millisecond)
		evt := RPCEvent{Type: "agent_end"}
		data, _ := json.Marshal(evt)
		data = append(data, '\n')
		ag.mu.Lock()
		ag.stdin.Write(data)
		ag.mu.Unlock()
	}()

	_ = ag.SendPrompt(ctx, "test")
	after := ag.LastActivity()

	if !after.After(before) {
		t.Error("LastActivity should be updated after SendPrompt")
	}
}

func TestAgentResponseRouting(t *testing.T) {
	bin := writeMockBinary(t)
	ag := NewAgent(bin, t.TempDir())
	ctx := context.Background()

	if err := ag.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ag.Stop()

	ch := make(chan *RPCEvent, 1)
	ag.pendingMu.Lock()
	ag.pending["42"] = ch
	ag.pendingMu.Unlock()

	evt := RPCEvent{Type: "response", ID: "42"}
	data, _ := json.Marshal(evt)
	data = append(data, '\n')
	ag.mu.Lock()
	ag.stdin.Write(data)
	ag.mu.Unlock()

	select {
	case resp := <-ch:
		if resp.Type != "response" || resp.ID != "42" {
			t.Errorf("unexpected response: %+v", resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for response")
	}
}

func TestAgentSendPromptAfterStop(t *testing.T) {
	bin := writeMockBinary(t)
	ag := NewAgent(bin, t.TempDir())
	ctx := context.Background()

	if err := ag.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ag.Stop()
	time.Sleep(100 * time.Millisecond)

	stream := ag.SendPrompt(ctx, "test")
	var gotErr error
	for evt := range stream {
		if evt.Err != nil {
			gotErr = evt.Err
			break
		}
	}
	if gotErr == nil {
		t.Fatal("expected error when sending to stopped agent")
	}
}

func TestAgentSendPromptContextCancel(t *testing.T) {
	bin := writeMockBinary(t)
	ag := NewAgent(bin, t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	if err := ag.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ag.Stop()

	// Cancel context after starting to read.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	stream := ag.SendPrompt(ctx, "test")
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
	cmd := RPCCommand{ID: "1", Type: "prompt", Message: "hello"}
	data, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded RPCCommand
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.ID != "1" || decoded.Type != "prompt" || decoded.Message != "hello" {
		t.Errorf("decoded = %+v, want ID=1 Type=prompt Message=hello", decoded)
	}
}

func TestRPCCommandOmitEmpty(t *testing.T) {
	cmd := RPCCommand{ID: "1", Type: "abort"}
	data, _ := json.Marshal(cmd)

	var m map[string]interface{}
	json.Unmarshal(data, &m)

	if _, ok := m["message"]; ok {
		t.Error("message field should be omitted when empty")
	}
}

func TestRPCEventUnmarshal(t *testing.T) {
	raw := `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"hi"}}`
	var evt RPCEvent
	if err := json.Unmarshal([]byte(raw), &evt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if evt.Type != "message_update" {
		t.Errorf("type = %q, want message_update", evt.Type)
	}

	var ame assistantMessageEvent
	if err := json.Unmarshal(evt.AssistantMessageEvent, &ame); err != nil {
		t.Fatalf("unmarshal assistant event: %v", err)
	}
	if ame.Type != "text_delta" || ame.Delta != "hi" {
		t.Errorf("ame = %+v, want text_delta/hi", ame)
	}
}
