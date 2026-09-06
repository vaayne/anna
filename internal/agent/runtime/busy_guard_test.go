package runtime

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/CherryHQ/stella/internal/agent/session"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/pkg/ai"
)

type blockingRunner struct {
	gate chan struct{}
}

func (r *blockingRunner) Chat(_ context.Context, _ []ai.Message, _ MessageContent) <-chan Event {
	out := make(chan Event)
	go func() {
		<-r.gate
		close(out)
	}()
	return out
}
func (r *blockingRunner) Alive() bool                  { return true }
func (r *blockingRunner) Busy() bool                   { return false }
func (r *blockingRunner) LastActivity() time.Time      { return time.Now() }
func (r *blockingRunner) SystemPrompt() string         { return "" }
func (r *blockingRunner) PluginContext() PluginContext { return PluginContext{} }
func (r *blockingRunner) Close() error                 { return nil }

func newTestRuntime(gate chan struct{}) *Runtime {
	mem := &recordingMemory{}
	rt, _ := New(Config{
		NewRunner: func(_ context.Context, _ RunnerParams) (Runner, error) {
			return &blockingRunner{gate: gate}, nil
		},
		Memory: mem,
	})
	return rt
}

func waitSessionFree(t *testing.T, rt *Runtime, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := rt.active.Load(sessionID); !ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("session %s did not become free", sessionID)
}

type activityRecordingMemory struct {
	recordingMemory
	activityMu sync.Mutex
	activity   []string
}

func (m *activityRecordingMemory) MarkSessionTurnStarted(_ context.Context, _ memory.Session) (bool, error) {
	m.activityMu.Lock()
	defer m.activityMu.Unlock()
	m.activity = append(m.activity, "started")
	return true, nil
}

func (m *activityRecordingMemory) MarkSessionTurnCompleted(_ context.Context, _ memory.Session, result memory.SessionTurnResult) (bool, error) {
	m.activityMu.Lock()
	defer m.activityMu.Unlock()
	m.activity = append(m.activity, "completed:"+string(result))
	return true, nil
}

func (m *activityRecordingMemory) MarkSessionViewed(context.Context, memory.Session) (bool, error) {
	return true, nil
}

func (m *activityRecordingMemory) activitySnapshot() []string {
	m.activityMu.Lock()
	defer m.activityMu.Unlock()
	return append([]string(nil), m.activity...)
}

func TestChatMarksTurnActivityAroundExecution(t *testing.T) {
	gate := make(chan struct{})
	mem := &activityRecordingMemory{}
	rt, err := New(Config{
		NewRunner: func(context.Context, RunnerParams) (Runner, error) {
			return &blockingRunner{gate: gate}, nil
		},
		Memory: mem,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	info := session.Info{ID: "activity-session", UserID: "u1", AgentID: "a1"}
	stream := rt.Chat(t.Context(), info, "hello")

	mem.activityMu.Lock()
	got := append([]string(nil), mem.activity...)
	mem.activityMu.Unlock()
	if !reflect.DeepEqual(got, []string{"started"}) {
		t.Fatalf("activity before completion = %v, want [started]", got)
	}

	close(gate)
	for range stream {
	}
	mem.activityMu.Lock()
	got = append([]string(nil), mem.activity...)
	mem.activityMu.Unlock()
	if !reflect.DeepEqual(got, []string{"started", "completed:success"}) {
		t.Fatalf("activity after completion = %v, want [started completed:success]", got)
	}
}

func TestChatMarksFailedTurnActivity(t *testing.T) {
	mem := &activityRecordingMemory{}
	rt, err := New(Config{
		NewRunner: func(context.Context, RunnerParams) (Runner, error) {
			return chatFakeRunner{events: []Event{{Err: errors.New("model failed")}}}, nil
		},
		Memory: mem,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream := rt.Chat(t.Context(), session.Info{ID: "failed-session", UserID: "u1", AgentID: "a1"}, "hello")
	for range stream {
	}
	if got := mem.activitySnapshot(); !reflect.DeepEqual(got, []string{"started", "completed:error"}) {
		t.Fatalf("failed turn activity = %v, want [started completed:error]", got)
	}
}

func TestChatAdmittedControlledFenceRejectsBeforeTurnSideEffects(t *testing.T) {
	gate := make(chan struct{})
	mem := &activityRecordingMemory{}
	rt, err := New(Config{
		NewRunner: func(context.Context, RunnerParams) (Runner, error) {
			return &blockingRunner{gate: gate}, nil
		},
		Memory: mem,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	info := session.Info{ID: "fenced-session", UserID: "u1", AgentID: "a1"}
	want := errors.New("caller timed out")
	if stream, err := rt.ChatAdmittedControlled(t.Context(), info, "must not run", func() error { return want }); !errors.Is(err, want) || stream != nil {
		t.Fatalf("controlled admission = (%v, %v), want (nil, %v)", stream, err, want)
	}
	if got := mem.activitySnapshot(); len(got) != 0 {
		t.Fatalf("rejected turn activity = %v, want none", got)
	}
	if _, active := rt.active.Load(info.ID); active {
		t.Fatal("rejected admission retained the runtime busy guard")
	}

	stream, err := rt.ChatAdmitted(t.Context(), info, "next turn")
	if err != nil {
		t.Fatalf("next admission: %v", err)
	}
	close(gate)
	for range stream {
	}
}

func TestChat_BusyGuard_RejectsConcurrentSameSession(t *testing.T) {
	gate := make(chan struct{})
	rt := newTestRuntime(gate)

	info := session.Info{
		ID:      "sess-1",
		UserID:  "u1",
		AgentID: "a1",
	}

	ch1 := rt.Chat(context.Background(), info, "hello")

	// Second chat on same session should be rejected immediately.
	ch2 := rt.Chat(context.Background(), info, "world")
	evt := <-ch2
	if evt.Err == nil || !errors.Is(evt.Err, ErrSessionBusy) {
		t.Fatalf("expected ErrSessionBusy, got %v", evt.Err)
	}

	// Release the first chat and drain. The output channel closes before the
	// outer Chat goroutine runs its active-session cleanup, so wait for the guard
	// itself to clear instead of racing the cleanup defer.
	close(gate)
	for range ch1 {
	}
	waitSessionFree(t, rt, info.ID)

	// After first completes, the session should be free again.
	gate2 := make(chan struct{})
	rt.cache.mu.Lock()
	for _, cs := range rt.cache.sessions {
		cs.r = &blockingRunner{gate: gate2}
	}
	rt.cache.mu.Unlock()

	ch3 := rt.Chat(context.Background(), info, "retry")
	// Give goroutine time to start.
	time.Sleep(10 * time.Millisecond)
	// Should not be busy — it should be running, not rejected.
	select {
	case evt := <-ch3:
		if evt.Err != nil && errors.Is(evt.Err, ErrSessionBusy) {
			t.Fatal("session should be free after first turn completes")
		}
	default:
		// Expected: ch3 is blocking because runner is active.
	}
	close(gate2)
	for range ch3 {
	}
}

type contextRunner struct{}

func (r *contextRunner) Chat(ctx context.Context, _ []ai.Message, _ MessageContent) <-chan Event {
	out := make(chan Event, 1)
	go func() {
		defer close(out)
		out <- Event{Text: "partial"}
		<-ctx.Done()
	}()
	return out
}
func (r *contextRunner) Alive() bool                  { return true }
func (r *contextRunner) Busy() bool                   { return false }
func (r *contextRunner) LastActivity() time.Time      { return time.Now() }
func (r *contextRunner) SystemPrompt() string         { return "" }
func (r *contextRunner) PluginContext() PluginContext { return PluginContext{} }
func (r *contextRunner) Close() error                 { return nil }

func TestStopSessionCancelsOnlyExplicitly(t *testing.T) {
	mem := &activityRecordingMemory{}
	rt, err := New(Config{
		NewRunner: func(_ context.Context, _ RunnerParams) (Runner, error) {
			return &contextRunner{}, nil
		},
		Memory: mem,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	info := session.Info{ID: "sess-stop", UserID: "u1", AgentID: "a1"}
	stream := rt.Chat(context.Background(), info, "hello")

	deadline := time.Now().Add(time.Second)
	for !rt.SessionLive(info.ID) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !rt.StopSession(t.Context(), info.ID) {
		t.Fatal("StopSession reported no active turn")
	}
	for range stream {
	}
	waitSessionFree(t, rt, info.ID)
	if rt.StopSession(t.Context(), info.ID) {
		t.Fatal("stopping an idle session should report false")
	}
	if got := mem.activitySnapshot(); !reflect.DeepEqual(got, []string{"started", "completed:canceled"}) {
		t.Fatalf("stopped turn activity = %v, want [started completed:canceled]", got)
	}
	mem.mu.Lock()
	defer mem.mu.Unlock()
	if len(mem.messages) != 2 {
		t.Fatalf("persisted messages = %d, want user plus partial assistant", len(mem.messages))
	}
	assistant, ok := mem.messages[1].(ai.AssistantMessage)
	if !ok || len(assistant.Content) != 1 {
		t.Fatalf("persisted partial assistant = %#v", mem.messages[1])
	}
	text, ok := assistant.Content[0].(ai.TextContent)
	if !ok || text.Text != "partial" {
		t.Fatalf("persisted partial text = %#v", assistant.Content[0])
	}
}

func TestChat_BusyGuard_AllowsDifferentSessions(t *testing.T) {
	gate := make(chan struct{})
	rt := newTestRuntime(gate)

	info1 := session.Info{ID: "sess-1", UserID: "u1", AgentID: "a1"}
	info2 := session.Info{ID: "sess-2", UserID: "u1", AgentID: "a1"}

	ch1 := rt.Chat(context.Background(), info1, "hello")
	ch2 := rt.Chat(context.Background(), info2, "world")

	// Neither should be rejected.
	time.Sleep(10 * time.Millisecond)
	select {
	case evt := <-ch2:
		if evt.Err != nil && errors.Is(evt.Err, ErrSessionBusy) {
			t.Fatal("different session should not be rejected")
		}
	default:
		// Expected: ch2 is running (not rejected).
	}

	close(gate)
	for range ch1 {
	}
	for range ch2 {
	}
}
