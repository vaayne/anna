package agent

import (
	"context"
	"testing"
	"time"
)

func TestNewSessionManager(t *testing.T) {
	bin := writeMockBinary(t)
	sm := NewSessionManager(bin, t.TempDir(), 10*time.Minute)
	if sm.binary != bin {
		t.Errorf("binary = %q, want %q", sm.binary, bin)
	}
	if sm.idleTimeout != 10*time.Minute {
		t.Errorf("idleTimeout = %v, want 10m", sm.idleTimeout)
	}
}

func TestSessionManagerGetOrCreate(t *testing.T) {
	dir := t.TempDir()
	// Use "cat" as mock binary — it starts and stays alive.
	sm := NewSessionManager(writeMockBinary(t), dir, 10*time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ag1, err := sm.GetOrCreate(ctx, "session-1")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	defer sm.StopAll()

	if !ag1.Alive() {
		t.Error("agent should be alive")
	}

	// Getting the same session should return the same agent.
	ag2, err := sm.GetOrCreate(ctx, "session-1")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if ag1 != ag2 {
		t.Error("expected same agent for same session ID")
	}
}

func TestSessionManagerMultipleSessions(t *testing.T) {
	dir := t.TempDir()
	sm := NewSessionManager(writeMockBinary(t), dir, 10*time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer sm.StopAll()

	ag1, err := sm.GetOrCreate(ctx, "a")
	if err != nil {
		t.Fatalf("GetOrCreate a: %v", err)
	}

	ag2, err := sm.GetOrCreate(ctx, "b")
	if err != nil {
		t.Fatalf("GetOrCreate b: %v", err)
	}

	if ag1 == ag2 {
		t.Error("different session IDs should return different agents")
	}
}

func TestSessionManagerReplacesDeadAgent(t *testing.T) {
	dir := t.TempDir()
	sm := NewSessionManager(writeMockBinary(t), dir, 10*time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer sm.StopAll()

	ag1, err := sm.GetOrCreate(ctx, "sess")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	// Kill the agent.
	ag1.Stop()
	time.Sleep(100 * time.Millisecond)

	// Next GetOrCreate should spawn a new one.
	ag2, err := sm.GetOrCreate(ctx, "sess")
	if err != nil {
		t.Fatalf("GetOrCreate after death: %v", err)
	}

	if ag1 == ag2 {
		t.Error("dead agent should be replaced with a new one")
	}
	if !ag2.Alive() {
		t.Error("replacement agent should be alive")
	}
}

func TestSessionManagerStopAll(t *testing.T) {
	dir := t.TempDir()
	sm := NewSessionManager(writeMockBinary(t), dir, 10*time.Minute)

	ctx := context.Background()

	ag1, _ := sm.GetOrCreate(ctx, "a")
	ag2, _ := sm.GetOrCreate(ctx, "b")

	sm.StopAll()

	time.Sleep(100 * time.Millisecond)
	if ag1.Alive() {
		t.Error("agent a should not be alive after StopAll")
	}
	if ag2.Alive() {
		t.Error("agent b should not be alive after StopAll")
	}
}

func TestSessionManagerReap(t *testing.T) {
	dir := t.TempDir()
	// Very short idle timeout for testing.
	sm := NewSessionManager(writeMockBinary(t), dir, 1*time.Millisecond)

	ctx := context.Background()
	defer sm.StopAll()

	ag, err := sm.GetOrCreate(ctx, "idle-sess")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	// Wait for agent to become idle.
	time.Sleep(50 * time.Millisecond)

	// Manually trigger reap.
	sm.reap()

	time.Sleep(100 * time.Millisecond)
	if ag.Alive() {
		t.Error("idle agent should be reaped")
	}

	// Session should be removed from the map.
	sm.mu.Lock()
	_, exists := sm.agents["idle-sess"]
	sm.mu.Unlock()
	if exists {
		t.Error("reaped session should be removed from agents map")
	}
}

func TestSessionManagerReapRemovesDead(t *testing.T) {
	dir := t.TempDir()
	sm := NewSessionManager(writeMockBinary(t), dir, 10*time.Minute)

	ctx := context.Background()

	ag, _ := sm.GetOrCreate(ctx, "dead-sess")
	ag.Stop()
	time.Sleep(100 * time.Millisecond)

	sm.reap()

	sm.mu.Lock()
	_, exists := sm.agents["dead-sess"]
	sm.mu.Unlock()
	if exists {
		t.Error("dead agent should be removed by reap")
	}
}

func TestSessionManagerStartReaperCancels(t *testing.T) {
	sm := NewSessionManager(writeMockBinary(t), t.TempDir(), 10*time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sm.StartReaper(ctx)
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

func TestSessionManagerInvalidBinary(t *testing.T) {
	sm := NewSessionManager("/nonexistent/binary", t.TempDir(), 10*time.Minute)
	_, err := sm.GetOrCreate(context.Background(), "test")
	if err == nil {
		t.Fatal("expected error for invalid binary")
	}
}
