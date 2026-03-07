package cron

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestAddListRemoveJob(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Stop()

	// Add a job.
	job, err := svc.AddJob("test", "say hello", Schedule{Every: "1h"}, "")
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if job.ID == "" {
		t.Fatal("expected non-empty job ID")
	}
	if job.Name != "test" {
		t.Errorf("Name = %q, want %q", job.Name, "test")
	}
	if !job.Enabled {
		t.Error("expected job to be enabled")
	}

	// List jobs.
	jobs := svc.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("ListJobs: got %d, want 1", len(jobs))
	}
	if jobs[0].ID != job.ID {
		t.Errorf("job ID = %q, want %q", jobs[0].ID, job.ID)
	}

	// Verify persistence file.
	data, err := os.ReadFile(filepath.Join(dir, "jobs.json"))
	if err != nil {
		t.Fatalf("read jobs.json: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("jobs.json is empty")
	}

	// Remove job.
	if err := svc.RemoveJob(job.ID); err != nil {
		t.Fatalf("RemoveJob: %v", err)
	}
	if jobs := svc.ListJobs(); len(jobs) != 0 {
		t.Errorf("ListJobs after remove: got %d, want 0", len(jobs))
	}
}

func TestAddJobValidation(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Stop()

	pastTime := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	tests := []struct {
		name    string
		jName   string
		message string
		sched   Schedule
	}{
		{"empty name", "", "msg", Schedule{Every: "1h"}},
		{"empty message", "test", "", Schedule{Every: "1h"}},
		{"no schedule", "test", "msg", Schedule{}},
		{"both cron and every", "test", "msg", Schedule{Cron: "* * * * *", Every: "1h"}},
		{"both cron and at", "test", "msg", Schedule{Cron: "* * * * *", At: time.Now().Add(time.Hour).Format(time.RFC3339)}},
		{"both every and at", "test", "msg", Schedule{Every: "1h", At: time.Now().Add(time.Hour).Format(time.RFC3339)}},
		{"all three set", "test", "msg", Schedule{Cron: "* * * * *", Every: "1h", At: time.Now().Add(time.Hour).Format(time.RFC3339)}},
		{"invalid duration", "test", "msg", Schedule{Every: "bogus"}},
		{"invalid at format", "test", "msg", Schedule{At: "not-a-timestamp"}},
		{"past at timestamp", "test", "msg", Schedule{At: pastTime}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.AddJob(tt.jName, tt.message, tt.sched, "")
			if err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestRemoveJobNotFound(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Stop()

	if err := svc.RemoveJob("nonexistent"); err == nil {
		t.Error("expected error for nonexistent job")
	}
}

func TestJobPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()

	// Create and add a job.
	svc1, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc1.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	job, err := svc1.AddJob("persist-test", "check weather", Schedule{Cron: "0 9 * * *"}, "")
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	svc1.Stop()

	// Create a new service from the same directory.
	svc2, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc2.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc2.Stop()

	jobs := svc2.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("ListJobs after restart: got %d, want 1", len(jobs))
	}
	if jobs[0].ID != job.ID {
		t.Errorf("job ID = %q, want %q", jobs[0].ID, job.ID)
	}
	if jobs[0].Name != "persist-test" {
		t.Errorf("job Name = %q, want %q", jobs[0].Name, "persist-test")
	}
}

func TestOnJobCallbackFires(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var mu sync.Mutex
	var fired []string
	svc.SetOnJob(func(_ context.Context, job Job) {
		mu.Lock()
		fired = append(fired, job.ID)
		mu.Unlock()
	})

	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Stop()

	_, err = svc.AddJob("quick", "ping", Schedule{Every: "100ms"}, "")
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Wait for the callback to fire.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(fired)
		mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("callback did not fire within 2s")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func TestCronToolAddListRemove(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Stop()

	ct := NewTool(svc)

	// Definition should have name "cron".
	def := ct.Definition()
	if def.Name != "cron" {
		t.Errorf("tool name = %q, want %q", def.Name, "cron")
	}

	// Add via tool.
	result, err := ct.Execute(context.Background(), map[string]any{
		"action":  "add",
		"name":    "tool-test",
		"message": "do something",
		"every":   "1h",
	})
	if err != nil {
		t.Fatalf("Execute add: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result from add")
	}

	// List via tool.
	result, err = ct.Execute(context.Background(), map[string]any{
		"action": "list",
	})
	if err != nil {
		t.Fatalf("Execute list: %v", err)
	}
	if result == "No scheduled jobs." {
		t.Error("expected jobs in list")
	}

	// Get the job ID for removal.
	jobs := svc.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}

	// Remove via tool.
	result, err = ct.Execute(context.Background(), map[string]any{
		"action": "remove",
		"id":     jobs[0].ID,
	})
	if err != nil {
		t.Fatalf("Execute remove: %v", err)
	}

	// Verify removed.
	result, err = ct.Execute(context.Background(), map[string]any{
		"action": "list",
	})
	if err != nil {
		t.Fatalf("Execute list after remove: %v", err)
	}
	if result != "No scheduled jobs." {
		t.Errorf("expected no jobs, got %q", result)
	}
}

func TestOneTimeJobCreation(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Stop()

	futureTime := time.Now().Add(1 * time.Hour).Format(time.RFC3339)
	job, err := svc.AddJob("one-time-test", "do something once", Schedule{At: futureTime}, "")
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if job.Schedule.At != futureTime {
		t.Errorf("At = %q, want %q", job.Schedule.At, futureTime)
	}

	jobs := svc.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("ListJobs: got %d, want 1", len(jobs))
	}
}

func TestOneTimeJobFiresAndAutoRemoves(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var mu sync.Mutex
	var fired []string
	svc.SetOnJob(func(_ context.Context, job Job) {
		mu.Lock()
		fired = append(fired, job.ID)
		mu.Unlock()
	})

	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Stop()

	// Schedule 200ms from now.
	at := time.Now().Add(200 * time.Millisecond).Format(time.RFC3339Nano)
	job, err := svc.AddJob("fire-once", "ping once", Schedule{At: at}, "")
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Wait for the callback to fire and cleanup to happen.
	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		n := len(fired)
		mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("callback did not fire within 3s")
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Verify the callback fired with the right job.
	mu.Lock()
	if fired[0] != job.ID {
		t.Errorf("fired job ID = %q, want %q", fired[0], job.ID)
	}
	mu.Unlock()

	// Wait a bit for the async cleanup goroutine.
	time.Sleep(200 * time.Millisecond)

	// Job should be auto-removed.
	jobs := svc.ListJobs()
	if len(jobs) != 0 {
		t.Errorf("ListJobs after one-time fire: got %d, want 0", len(jobs))
	}
}

func TestOneTimeJobSkippedOnRestartIfPast(t *testing.T) {
	dir := t.TempDir()

	// Create a service and add a one-time job in the future.
	svc1, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc1.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	futureTime := time.Now().Add(1 * time.Hour).Format(time.RFC3339)
	_, err = svc1.AddJob("restart-test", "do once", Schedule{At: futureTime}, "")
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	svc1.Stop()

	// Manually tamper the job to have a past timestamp to simulate missed window.
	jobs, err := svc1.loadJobs()
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	jobs[0].Schedule.At = time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	svc1.mu.Lock()
	svc1.jobs[jobs[0].ID] = jobs[0]
	_ = svc1.saveJobsLocked()
	svc1.mu.Unlock()

	// Restart: the past one-time job should be loaded but not scheduled (silently skipped).
	svc2, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc2.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc2.Stop()

	// Job is still in the list (persisted) but not scheduled with gocron.
	listed := svc2.ListJobs()
	if len(listed) != 1 {
		t.Fatalf("expected 1 persisted job, got %d", len(listed))
	}

	svc2.mu.Lock()
	_, hasGID := svc2.gids[listed[0].ID]
	svc2.mu.Unlock()
	if hasGID {
		t.Error("expected past one-time job to not be scheduled with gocron")
	}
}

func TestCronToolAddOneTimeJob(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Stop()

	ct := NewTool(svc)

	futureTime := time.Now().Add(1 * time.Hour).Format(time.RFC3339)
	result, err := ct.Execute(context.Background(), map[string]any{
		"action":  "add",
		"name":    "reminder",
		"message": "check the weather",
		"at":      futureTime,
	})
	if err != nil {
		t.Fatalf("Execute add one-time: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result")
	}

	jobs := svc.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].Schedule.At != futureTime {
		t.Errorf("At = %q, want %q", jobs[0].Schedule.At, futureTime)
	}
}

func TestSessionModeDefault(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Stop()

	// Empty session_mode defaults to "reuse".
	job, err := svc.AddJob("default-mode", "msg", Schedule{Every: "1h"}, "")
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if job.SessionMode != SessionReuse {
		t.Errorf("SessionMode = %q, want %q", job.SessionMode, SessionReuse)
	}
}

func TestSessionModeReuse(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Stop()

	job, err := svc.AddJob("reuse-mode", "msg", Schedule{Every: "1h"}, SessionReuse)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Reuse mode: SessionID is stable across calls.
	id1 := job.SessionID()
	id2 := job.SessionID()
	if id1 != id2 {
		t.Errorf("reuse mode: SessionID changed: %q vs %q", id1, id2)
	}
	if id1 != "cron:"+job.ID {
		t.Errorf("reuse mode: SessionID = %q, want %q", id1, "cron:"+job.ID)
	}
}

func TestSessionModeNew(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Stop()

	job, err := svc.AddJob("new-mode", "msg", Schedule{Every: "1h"}, SessionNew)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if job.SessionMode != SessionNew {
		t.Errorf("SessionMode = %q, want %q", job.SessionMode, SessionNew)
	}

	// New mode: SessionID differs across calls.
	id1 := job.SessionID()
	time.Sleep(1 * time.Millisecond) // ensure different nano timestamp
	id2 := job.SessionID()
	if id1 == id2 {
		t.Error("new mode: SessionID should differ between calls")
	}
}

func TestSessionModeInvalid(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Stop()

	_, err = svc.AddJob("bad-mode", "msg", Schedule{Every: "1h"}, "invalid")
	if err == nil {
		t.Error("expected error for invalid session_mode")
	}
}

func TestCronToolSessionMode(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Stop()

	ct := NewTool(svc)

	// Add with session_mode "new" via tool.
	_, err = ct.Execute(context.Background(), map[string]any{
		"action":       "add",
		"name":         "fresh-session",
		"message":      "do work",
		"every":        "1h",
		"session_mode": "new",
	})
	if err != nil {
		t.Fatalf("Execute add: %v", err)
	}

	jobs := svc.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].SessionMode != SessionNew {
		t.Errorf("SessionMode = %q, want %q", jobs[0].SessionMode, SessionNew)
	}
}

func TestCronToolInvalidAction(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ct := NewTool(svc)

	_, err = ct.Execute(context.Background(), map[string]any{
		"action": "invalid",
	})
	if err == nil {
		t.Error("expected error for invalid action")
	}
}

func TestCronToolRemoveMissingID(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ct := NewTool(svc)

	_, err = ct.Execute(context.Background(), map[string]any{
		"action": "remove",
	})
	if err == nil {
		t.Error("expected error for missing ID")
	}
}
