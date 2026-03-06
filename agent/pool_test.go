package agent

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// mockRunner implements Runner and io.Closer for pool tests.
type mockRunner struct {
	mu           sync.Mutex
	events       []Event
	closed       bool
	lastActivity time.Time
	alive        bool
}

func newMockRunner(events []Event) *mockRunner {
	return &mockRunner{
		events:       events,
		lastActivity: time.Now(),
		alive:        true,
	}
}

func (m *mockRunner) Chat(_ context.Context, _ []RPCEvent, _ string) <-chan Event {
	m.mu.Lock()
	m.lastActivity = time.Now()
	events := m.events
	m.mu.Unlock()

	out := make(chan Event, len(events))
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
func mockRunnerFactory(events []Event) (NewRunnerFunc, *[]*mockRunner) {
	var runners []*mockRunner
	var mu sync.Mutex
	factory := func(_ context.Context) (Runner, error) {
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
	events := []Event{
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
	factory, runners := mockRunnerFactory([]Event{{Text: "ok"}})
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
	factory, runners := mockRunnerFactory([]Event{{Text: "ok"}})
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
	events := []Event{
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

	if histLen != 2 {
		t.Errorf("history length = %d, want 2", histLen)
	}
}

func TestPoolChatErrorFromFactory(t *testing.T) {
	factory := func(_ context.Context) (Runner, error) {
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
	factory, runners := mockRunnerFactory([]Event{{Text: "ok"}})
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
	factory, runners := mockRunnerFactory([]Event{{Text: "ok"}})
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
	bin := writeMockBinary(t)
	factory := func(ctx context.Context) (Runner, error) {
		return NewProcessRunner(ctx, bin, "")
	}
	pool := NewPool(factory, WithIdleTimeout(1*time.Millisecond))
	defer pool.Close()

	ctx := context.Background()

	// Create a session by triggering getOrCreateRunner.
	_, runner, err := pool.getOrCreateRunner(ctx, "idle-sess")
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

	pr := runner.(*ProcessRunner)
	if pr.Alive() {
		t.Error("idle runner should not be alive after reap")
	}
}

func TestPoolReapDead(t *testing.T) {
	bin := writeMockBinary(t)
	factory := func(ctx context.Context) (Runner, error) {
		return NewProcessRunner(ctx, bin, "")
	}
	pool := NewPool(factory, WithIdleTimeout(10*time.Minute))
	defer pool.Close()

	ctx := context.Background()

	// Create a session with a real ProcessRunner.
	_, runner, err := pool.getOrCreateRunner(ctx, "dead-sess")
	if err != nil {
		t.Fatalf("getOrCreateRunner: %v", err)
	}

	// Kill the runner.
	runner.(*ProcessRunner).Close()
	time.Sleep(100 * time.Millisecond)

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
	// Use real ProcessRunner to test dead-runner replacement in getOrCreateRunner.
	bin := writeMockBinary(t)
	factory := func(ctx context.Context) (Runner, error) {
		return NewProcessRunner(ctx, bin, "")
	}
	pool := NewPool(factory)
	defer pool.Close()

	ctx := context.Background()

	// Create a session with a runner.
	pool.mu.Lock()
	pool.sessions["sess"] = &Session{}
	pool.mu.Unlock()

	// Use pool.Chat to create the runner via getOrCreateRunner.
	// First, just trigger runner creation.
	r, runnerRef, err := pool.getOrCreateRunner(ctx, "sess")
	if err != nil {
		t.Fatalf("getOrCreateRunner: %v", err)
	}
	_ = r

	// Kill the runner.
	if closer, ok := runnerRef.(io.Closer); ok {
		closer.Close()
	}
	time.Sleep(100 * time.Millisecond)

	// Next call should create a new runner.
	_, runner2, err := pool.getOrCreateRunner(ctx, "sess")
	if err != nil {
		t.Fatalf("getOrCreateRunner after death: %v", err)
	}

	if runner2 == runnerRef {
		t.Error("dead runner should be replaced with a new one")
	}
}
