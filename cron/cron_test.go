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
	job, err := svc.AddJob("test", "say hello", Schedule{Every: "1h"})
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
		{"invalid duration", "test", "msg", Schedule{Every: "bogus"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.AddJob(tt.jName, tt.message, tt.sched)
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
	job, err := svc1.AddJob("persist-test", "check weather", Schedule{Cron: "0 9 * * *"})
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

	_, err = svc.AddJob("quick", "ping", Schedule{Every: "100ms"})
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
