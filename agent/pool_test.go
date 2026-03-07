package agent

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/vaayne/anna/agent/runner"
	"github.com/vaayne/anna/agent/store"
)

// mockRunner implements runner.Runner and io.Closer for pool tests.
type mockRunner struct {
	mu           sync.Mutex
	events       []runner.Event
	closed       bool
	lastActivity time.Time
	alive        bool
}

func newMockRunner(events []runner.Event) *mockRunner {
	return &mockRunner{
		events:       events,
		lastActivity: time.Now(),
		alive:        true,
	}
}

func (m *mockRunner) Chat(_ context.Context, _ []runner.RPCEvent, _ string) <-chan runner.Event {
	m.mu.Lock()
	m.lastActivity = time.Now()
	events := m.events
	m.mu.Unlock()

	out := make(chan runner.Event, len(events))
	go func() {
		defer close(out)
		for _, evt := range events {
			out <- evt
		}
	}()
	return out
}

func (m *mockRunner) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.alive = false
	return nil
}

func (m *mockRunner) Alive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.alive
}

func (m *mockRunner) LastActivity() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastActivity
}

// mockRunnerFactory returns a NewRunnerFunc that creates mockRunners with the
// given canned events. It also tracks all created runners.
func mockRunnerFactory(events []runner.Event) (runner.NewRunnerFunc, *[]*mockRunner) {
	var runners []*mockRunner
	var mu sync.Mutex
	factory := func(_ context.Context, _ string) (runner.Runner, error) {
		r := newMockRunner(events)
		mu.Lock()
		runners = append(runners, r)
		mu.Unlock()
		return r, nil
	}
	return factory, &runners
}

func TestNewPool(t *testing.T) {
	factory, _ := mockRunnerFactory(nil)
	pool := NewPool(factory, WithIdleTimeout(5*time.Minute))

	if pool.idleTimeout != 5*time.Minute {
		t.Errorf("idleTimeout = %v, want 5m", pool.idleTimeout)
	}
}

func TestPoolChat(t *testing.T) {
	events := []runner.Event{
		{Text: "Hello "},
		{Text: "world"},
	}
	factory, _ := mockRunnerFactory(events)
	pool := NewPool(factory)
	defer pool.Close()

	ctx := context.Background()
	stream := pool.Chat(ctx, "session-1", "test")

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

func TestPoolChatReusesSession(t *testing.T) {
	factory, runners := mockRunnerFactory([]runner.Event{{Text: "ok"}})
	pool := NewPool(factory)
	defer pool.Close()

	ctx := context.Background()

	// First chat creates a runner.
	stream := pool.Chat(ctx, "sess-1", "first")
	for range stream {
	}

	// Second chat should reuse the same runner.
	stream = pool.Chat(ctx, "sess-1", "second")
	for range stream {
	}

	if len(*runners) != 1 {
		t.Errorf("expected 1 runner created, got %d", len(*runners))
	}
}

func TestPoolChatMultipleSessions(t *testing.T) {
	factory, runners := mockRunnerFactory([]runner.Event{{Text: "ok"}})
	pool := NewPool(factory)
	defer pool.Close()

	ctx := context.Background()

	stream := pool.Chat(ctx, "a", "msg")
	for range stream {
	}

	stream = pool.Chat(ctx, "b", "msg")
	for range stream {
	}

	if len(*runners) != 2 {
		t.Errorf("expected 2 runners created, got %d", len(*runners))
	}
}

func TestPoolChatAccumulatesHistory(t *testing.T) {
	events := []runner.Event{
		{Text: "chunk1"},
		{Text: "chunk2"},
	}
	factory, _ := mockRunnerFactory(events)
	pool := NewPool(factory)
	defer pool.Close()

	ctx := context.Background()

	stream := pool.Chat(ctx, "sess", "msg")
	for range stream {
	}

	pool.mu.Lock()
	sess := pool.sessions["sess"]
	histLen := len(sess.Events)
	pool.mu.Unlock()

	// 1 user_message + 2 text deltas = 3 events.
	if histLen != 3 {
		t.Errorf("history length = %d, want 3", histLen)
	}
}

func TestPoolChatErrorFromFactory(t *testing.T) {
	factory := func(_ context.Context, _ string) (runner.Runner, error) {
		return nil, fmt.Errorf("factory error")
	}
	pool := NewPool(factory)
	defer pool.Close()

	stream := pool.Chat(context.Background(), "sess", "msg")

	var gotErr error
	for evt := range stream {
		if evt.Err != nil {
			gotErr = evt.Err
			break
		}
	}

	if gotErr == nil {
		t.Fatal("expected error from factory")
	}
}

func TestPoolReset(t *testing.T) {
	factory, runners := mockRunnerFactory([]runner.Event{{Text: "ok"}})
	pool := NewPool(factory)
	defer pool.Close()

	ctx := context.Background()

	stream := pool.Chat(ctx, "sess", "msg")
	for range stream {
	}

	if err := pool.Reset("sess"); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	// The old runner should be closed.
	if !(*runners)[0].closed {
		t.Error("old runner should be closed after Reset")
	}

	// Session should be removed.
	pool.mu.Lock()
	_, exists := pool.sessions["sess"]
	pool.mu.Unlock()
	if exists {
		t.Error("session should be removed after Reset")
	}

	// Next chat should create a new runner.
	stream = pool.Chat(ctx, "sess", "msg2")
	for range stream {
	}

	if len(*runners) != 2 {
		t.Errorf("expected 2 runners, got %d", len(*runners))
	}
}

func TestPoolResetNonexistent(t *testing.T) {
	factory, _ := mockRunnerFactory(nil)
	pool := NewPool(factory)

	// Should not error on nonexistent session.
	if err := pool.Reset("nonexistent"); err != nil {
		t.Fatalf("Reset nonexistent: %v", err)
	}
}

func TestPoolClose(t *testing.T) {
	factory, runners := mockRunnerFactory([]runner.Event{{Text: "ok"}})
	pool := NewPool(factory)

	ctx := context.Background()

	stream := pool.Chat(ctx, "a", "msg")
	for range stream {
	}
	stream = pool.Chat(ctx, "b", "msg")
	for range stream {
	}

	if err := pool.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for i, r := range *runners {
		if !r.closed {
			t.Errorf("runner %d should be closed after pool.Close()", i)
		}
	}

	pool.mu.Lock()
	sessCount := len(pool.sessions)
	pool.mu.Unlock()
	if sessCount != 0 {
		t.Errorf("sessions count = %d, want 0", sessCount)
	}
}

func TestPoolReapIdle(t *testing.T) {
	factory, _ := mockRunnerFactory([]runner.Event{{Text: "ok"}})
	pool := NewPool(factory, WithIdleTimeout(1*time.Millisecond))
	defer pool.Close()

	ctx := context.Background()

	// Create a session by triggering getOrCreateRunner.
	_, r, err := pool.getOrCreateRunner(ctx, "idle-sess", "")
	if err != nil {
		t.Fatalf("getOrCreateRunner: %v", err)
	}

	// Wait for the runner to become idle.
	time.Sleep(50 * time.Millisecond)

	// Manually trigger reap.
	pool.reap()

	time.Sleep(100 * time.Millisecond)

	// Runner should be closed (nil'd out), but session still exists.
	pool.mu.Lock()
	sess, exists := pool.sessions["idle-sess"]
	var runnerNil bool
	if exists {
		runnerNil = sess.Runner == nil
	}
	pool.mu.Unlock()

	if !exists {
		t.Error("session should still exist after reap (history preserved)")
	}
	if !runnerNil {
		t.Error("runner should be nil after reap")
	}

	mr := r.(*mockRunner)
	if mr.Alive() {
		t.Error("idle runner should not be alive after reap")
	}
}

func TestPoolReapDead(t *testing.T) {
	factory, runners := mockRunnerFactory([]runner.Event{{Text: "ok"}})
	pool := NewPool(factory, WithIdleTimeout(10*time.Minute))
	defer pool.Close()

	ctx := context.Background()

	// Create a session with a mockRunner.
	_, _, err := pool.getOrCreateRunner(ctx, "dead-sess", "")
	if err != nil {
		t.Fatalf("getOrCreateRunner: %v", err)
	}

	// Kill the runner by marking it dead.
	(*runners)[0].mu.Lock()
	(*runners)[0].alive = false
	(*runners)[0].mu.Unlock()

	pool.reap()

	pool.mu.Lock()
	sess, exists := pool.sessions["dead-sess"]
	var runnerNil bool
	if exists {
		runnerNil = sess.Runner == nil
	}
	pool.mu.Unlock()

	if !exists {
		t.Error("session should still exist after reap of dead runner")
	}
	if !runnerNil {
		t.Error("dead runner should be nil'd after reap")
	}
}

func TestPoolStartReaperCancels(t *testing.T) {
	factory, _ := mockRunnerFactory(nil)
	pool := NewPool(factory)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pool.StartReaper(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// OK, StartReaper returned.
	case <-time.After(2 * time.Second):
		t.Fatal("StartReaper did not return after context cancel")
	}
}

func TestPoolReplacesDeadRunnerOnChat(t *testing.T) {
	// Use mockRunner to test dead-runner replacement in getOrCreateRunner.
	factory, runners := mockRunnerFactory([]runner.Event{{Text: "ok"}})
	pool := NewPool(factory)
	defer pool.Close()

	ctx := context.Background()

	// Create a session with a runner.
	_, _, err := pool.getOrCreateRunner(ctx, "sess", "")
	if err != nil {
		t.Fatalf("getOrCreateRunner: %v", err)
	}

	// Kill the runner by marking it dead.
	(*runners)[0].mu.Lock()
	(*runners)[0].alive = false
	(*runners)[0].mu.Unlock()

	// Next call should create a new runner.
	_, runner2, err := pool.getOrCreateRunner(ctx, "sess", "")
	if err != nil {
		t.Fatalf("getOrCreateRunner after death: %v", err)
	}

	if runner2 == (*runners)[0] {
		t.Error("dead runner should be replaced with a new one")
	}
	if len(*runners) != 2 {
		t.Errorf("expected 2 runners created, got %d", len(*runners))
	}
}

func TestPoolCreateSession(t *testing.T) {
	factory, _ := mockRunnerFactory(nil)
	pool := NewPool(factory)
	defer pool.Close()

	info, err := pool.CreateSession()
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if info.ID == "" {
		t.Error("session ID should not be empty")
	}
	if info.Archived {
		t.Error("new session should not be archived")
	}
	if info.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set")
	}
}

func TestPoolCreateAndListSessions(t *testing.T) {
	factory, _ := mockRunnerFactory(nil)
	pool := NewPool(factory)
	defer pool.Close()

	pool.CreateSession()
	pool.CreateSession()

	sessions, err := pool.ListSessions(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Errorf("expected 2 sessions, got %d", len(sessions))
	}
}

func TestPoolArchiveSession(t *testing.T) {
	factory, runners := mockRunnerFactory([]runner.Event{{Text: "ok"}})
	pool := NewPool(factory)
	defer pool.Close()

	info, _ := pool.CreateSession()

	// Chat to create a runner
	stream := pool.Chat(context.Background(), info.ID, "test")
	for range stream {
	}

	if err := pool.ArchiveSession(info.ID); err != nil {
		t.Fatalf("ArchiveSession: %v", err)
	}

	// Runner should be closed
	if !(*runners)[0].closed {
		t.Error("runner should be closed after archive")
	}

	// Session should be removed from memory
	pool.mu.Lock()
	_, exists := pool.sessions[info.ID]
	pool.mu.Unlock()
	if exists {
		t.Error("session should be removed from memory after archive")
	}
}

func TestPoolGetSession(t *testing.T) {
	factory, _ := mockRunnerFactory(nil)
	pool := NewPool(factory)
	defer pool.Close()

	info, _ := pool.CreateSession()

	got, err := pool.GetSession(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != info.ID {
		t.Errorf("got ID %q, want %q", got.ID, info.ID)
	}
}

func TestPoolGetSessionNotFound(t *testing.T) {
	factory, _ := mockRunnerFactory(nil)
	pool := NewPool(factory)

	_, err := pool.GetSession("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestPoolChatAutoTitles(t *testing.T) {
	factory, _ := mockRunnerFactory([]runner.Event{{Text: "response"}})
	pool := NewPool(factory)
	defer pool.Close()

	info, _ := pool.CreateSession()

	stream := pool.Chat(context.Background(), info.ID, "How do I fix the bug in pool.go?")
	for range stream {
	}

	pool.mu.Lock()
	sess := pool.sessions[info.ID]
	title := sess.Info.Title
	pool.mu.Unlock()

	if title == "" {
		t.Error("session should have auto-generated title")
	}
	if title != "How do I fix the bug in pool.go?" {
		t.Errorf("unexpected title: %q", title)
	}
}

func TestPoolChatAutoTitleTruncates(t *testing.T) {
	factory, _ := mockRunnerFactory([]runner.Event{{Text: "ok"}})
	pool := NewPool(factory)
	defer pool.Close()

	info, _ := pool.CreateSession()

	longMsg := "This is a very long message that should be truncated at a word boundary to keep the title reasonable and readable"
	stream := pool.Chat(context.Background(), info.ID, longMsg)
	for range stream {
	}

	pool.mu.Lock()
	title := pool.sessions[info.ID].Info.Title
	pool.mu.Unlock()

	if len(title) > 65 { // 60 + "…"
		t.Errorf("title too long (%d chars): %q", len(title), title)
	}
}

func TestPoolChatWithModel(t *testing.T) {
	// Track which model was requested for each runner creation.
	var models []string
	var mu sync.Mutex
	factory := func(_ context.Context, model string) (runner.Runner, error) {
		mu.Lock()
		models = append(models, model)
		mu.Unlock()
		return newMockRunner([]runner.Event{{Text: "ok"}}), nil
	}

	pool := NewPool(factory, WithDefaultModel("default-model"))
	defer pool.Close()

	ctx := context.Background()
	info, _ := pool.CreateSession()

	// First chat uses default model.
	stream := pool.Chat(ctx, info.ID, "hello")
	for range stream {
	}

	mu.Lock()
	if len(models) != 1 || models[0] != "default-model" {
		t.Fatalf("first call models = %v, want [default-model]", models)
	}
	mu.Unlock()

	// Second chat with explicit model triggers runner replacement.
	stream = pool.Chat(ctx, info.ID, "hello", WithModel("custom-model"))
	for range stream {
	}

	mu.Lock()
	if len(models) != 2 || models[1] != "custom-model" {
		t.Fatalf("second call models = %v, want [..., custom-model]", models)
	}
	mu.Unlock()

	// Third chat without model reuses the session's current model (custom-model).
	stream = pool.Chat(ctx, info.ID, "hello")
	for range stream {
	}

	mu.Lock()
	// No new runner should be created — still 2 total.
	if len(models) != 2 {
		t.Fatalf("third call created new runner, models = %v, want len 2", models)
	}
	mu.Unlock()
}

func TestPoolFastModelForCompaction(t *testing.T) {
	var models []string
	var mu sync.Mutex
	factory := func(_ context.Context, model string) (runner.Runner, error) {
		mu.Lock()
		models = append(models, model)
		mu.Unlock()
		return newMockRunner([]runner.Event{{Text: "summary text"}}), nil
	}

	dir := t.TempDir()
	s, _ := store.NewFileStore(dir, t.TempDir())
	pool := NewPool(factory,
		WithDefaultModel("strong-model"),
		WithFastModel("fast-model"),
		WithStore(s),
	)
	defer pool.Close()

	info, _ := pool.CreateSession()

	// Chat to create a session with history.
	stream := pool.Chat(context.Background(), info.ID, "hello")
	for range stream {
	}

	mu.Lock()
	if models[0] != "strong-model" {
		t.Fatalf("chat model = %q, want strong-model", models[0])
	}
	mu.Unlock()

	// Compact should use fast model.
	_, err := pool.CompactSession(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("CompactSession: %v", err)
	}

	mu.Lock()
	// The compaction should have created a runner with fast-model.
	found := false
	for _, m := range models {
		if m == "fast-model" {
			found = true
			break
		}
	}
	mu.Unlock()

	if !found {
		t.Errorf("compaction did not use fast model, models = %v", models)
	}

	// After compaction, session model should be restored to strong-model
	// so subsequent chats don't stay on the fast tier.
	pool.mu.Lock()
	sessModel := pool.sessions[info.ID].Model
	pool.mu.Unlock()

	if sessModel != "strong-model" {
		t.Errorf("session model after compaction = %q, want %q", sessModel, "strong-model")
	}
}

func TestSetDefaultModelAffectsNewSessions(t *testing.T) {
	var models []string
	var mu sync.Mutex
	factory := func(_ context.Context, model string) (runner.Runner, error) {
		mu.Lock()
		models = append(models, model)
		mu.Unlock()
		return newMockRunner([]runner.Event{{Text: "ok"}}), nil
	}

	pool := NewPool(factory, WithDefaultModel("initial-model"))
	defer pool.Close()

	ctx := context.Background()

	// First session uses initial default.
	info1, _ := pool.CreateSession()
	stream := pool.Chat(ctx, info1.ID, "hello")
	for range stream {
	}

	mu.Lock()
	if models[0] != "initial-model" {
		t.Fatalf("first session model = %q, want initial-model", models[0])
	}
	mu.Unlock()

	// Switch default model at runtime.
	pool.SetDefaultModel("switched-model")

	// New session should use the switched model.
	info2, _ := pool.CreateSession()
	stream = pool.Chat(ctx, info2.ID, "hello")
	for range stream {
	}

	mu.Lock()
	if models[1] != "switched-model" {
		t.Fatalf("second session model = %q, want switched-model", models[1])
	}
	mu.Unlock()
}
