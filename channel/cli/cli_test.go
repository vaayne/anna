package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/vaayne/anna/agent"
)

// initModel creates a chatModel and sends an initial WindowSizeMsg so the viewport is ready.
func initModel(t *testing.T, sm agent.SessionProvider) chatModel {
	t.Helper()
	m := newChatModel(context.Background(), sm)
	result, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return result.(chatModel)
}

func writeMockPi(t *testing.T, events []map[string]interface{}) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "mock-pi")

	var lines []string
	for _, evt := range events {
		data, _ := json.Marshal(evt)
		lines = append(lines, fmt.Sprintf("echo '%s'", string(data)))
	}

	script := "#!/bin/sh\nread line\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestChatModelQuit(t *testing.T) {
	bin := writeMockPi(t, []map[string]interface{}{
		{"type": "agent_end"},
	})
	sm := agent.NewSessionManager(bin, "", t.TempDir(), 10*time.Minute)
	defer sm.StopAll()

	m := initModel(t, sm)

	// Type "/quit" into textarea then press Enter.
	m.textarea.SetValue("/quit")
	result, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_ = result

	// The cmd should be tea.Quit.
	if cmd == nil {
		t.Fatal("expected quit command, got nil")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", msg)
	}
}

func TestChatModelExit(t *testing.T) {
	bin := writeMockPi(t, []map[string]interface{}{
		{"type": "agent_end"},
	})
	sm := agent.NewSessionManager(bin, "", t.TempDir(), 10*time.Minute)
	defer sm.StopAll()

	m := initModel(t, sm)

	m.textarea.SetValue("/exit")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if cmd == nil {
		t.Fatal("expected quit command, got nil")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", msg)
	}
}

func TestChatModelNewSession(t *testing.T) {
	bin := writeMockPi(t, []map[string]interface{}{
		{"type": "agent_end"},
	})
	sm := agent.NewSessionManager(bin, "", t.TempDir(), 10*time.Minute)
	defer sm.StopAll()

	m := initModel(t, sm)

	m.textarea.SetValue("/new")
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated := result.(chatModel)

	if !strings.Contains(updated.history.String(), "new session started") {
		t.Errorf("expected new session message in history, got: %s", updated.history.String())
	}
}

func TestChatModelSkipsEmpty(t *testing.T) {
	bin := writeMockPi(t, []map[string]interface{}{
		{"type": "agent_end"},
	})
	sm := agent.NewSessionManager(bin, "", t.TempDir(), 10*time.Minute)
	defer sm.StopAll()

	m := initModel(t, sm)

	// Empty input — pressing Enter with nothing typed.
	m.textarea.SetValue("")
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated := result.(chatModel)

	if updated.streaming {
		t.Error("should not be streaming on empty input")
	}
	if updated.history.Len() > 0 {
		t.Error("expected empty history for empty input")
	}
}

func TestChatModelStreaming(t *testing.T) {
	bin := writeMockPi(t, []map[string]interface{}{
		{"type": "agent_end"},
	})
	sm := agent.NewSessionManager(bin, "", t.TempDir(), 10*time.Minute)
	defer sm.StopAll()

	m := initModel(t, sm)

	// Send a prompt — puts model into streaming state.
	m.textarea.SetValue("hello")
	result, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = result.(chatModel)

	if !m.streaming {
		t.Error("expected streaming to be true after sending prompt")
	}
	if cmd == nil {
		t.Fatal("expected a command to start streaming")
	}
	if !strings.Contains(m.history.String(), "You") {
		t.Error("expected user message in history")
	}

	// Simulate streamStartMsg with a fake channel.
	ch := make(chan agent.StreamEvent, 3)
	ch <- agent.StreamEvent{Text: "Hello"}
	ch <- agent.StreamEvent{Text: " world"}
	close(ch)

	result, cmd = m.Update(streamStartMsg{stream: ch})
	m = result.(chatModel)

	// Drain all chunks.
	for cmd != nil {
		msg := cmd()
		result, cmd = m.Update(msg)
		m = result.(chatModel)
	}

	if m.streaming {
		t.Error("expected streaming to be false after stream done")
	}
	if !strings.Contains(m.history.String(), "Hello world") {
		t.Errorf("expected 'Hello world' in history, got: %s", m.history.String())
	}
}

func TestChatModelCtrlCQuits(t *testing.T) {
	bin := writeMockPi(t, []map[string]interface{}{
		{"type": "agent_end"},
	})
	sm := agent.NewSessionManager(bin, "", t.TempDir(), 10*time.Minute)
	defer sm.StopAll()

	m := initModel(t, sm)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("expected quit command on ctrl+c")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", msg)
	}
}
