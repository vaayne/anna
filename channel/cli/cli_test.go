package cli

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/vaayne/anna/agent"
	"github.com/vaayne/anna/agent/runner"
)

// mockRunner implements runner.Runner for testing.
type mockRunner struct {
	events []runner.Event
}

func (m *mockRunner) Chat(_ context.Context, _ []runner.RPCEvent, _ string) <-chan runner.Event {
	ch := make(chan runner.Event, len(m.events))
	for _, e := range m.events {
		ch <- e
	}
	close(ch)
	return ch
}

func newTestPool(events []runner.Event) *agent.Pool {
	factory := func(_ context.Context) (runner.Runner, error) {
		return &mockRunner{events: events}, nil
	}
	return agent.NewPool(factory)
}

// initModel creates a chatModel and sends an initial WindowSizeMsg so the viewport is ready.
func initModel(t *testing.T, pool *agent.Pool) chatModel {
	t.Helper()
	m := newChatModel(context.Background(), pool, "test", "test-model", nil, nil)
	result, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return result.(chatModel)
}

func TestChatModelQuit(t *testing.T) {
	pool := newTestPool(nil)
	defer pool.Close()

	m := initModel(t, pool)

	m.textarea.SetValue("/quit")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if cmd == nil {
		t.Fatal("expected quit command, got nil")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", msg)
	}
}

func TestChatModelExit(t *testing.T) {
	pool := newTestPool(nil)
	defer pool.Close()

	m := initModel(t, pool)

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
	pool := newTestPool(nil)
	defer pool.Close()

	m := initModel(t, pool)

	m.textarea.SetValue("/new")
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated := result.(chatModel)

	if !strings.Contains(updated.history.String(), "new session started") {
		t.Errorf("expected new session message in history, got: %s", updated.history.String())
	}
}

func TestChatModelSkipsEmpty(t *testing.T) {
	pool := newTestPool(nil)
	defer pool.Close()

	m := initModel(t, pool)

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
	pool := newTestPool(nil)
	defer pool.Close()

	m := initModel(t, pool)

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
	ch := make(chan runner.Event, 3)
	ch <- runner.Event{Text: "Hello"}
	ch <- runner.Event{Text: " world"}
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
	pool := newTestPool(nil)
	defer pool.Close()

	m := initModel(t, pool)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("expected quit command on ctrl+c")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", msg)
	}
}
