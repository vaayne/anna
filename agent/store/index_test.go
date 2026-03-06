package store

import (
	"os"
	"testing"
	"time"
)

func TestIndexSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir, "/tmp")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().Truncate(time.Millisecond)
	info := SessionInfo{
		ID:         "test-1",
		Title:      "Test session",
		CreatedAt:  now,
		LastActive: now,
	}

	if err := s.SaveInfo(info); err != nil {
		t.Fatal(err)
	}

	got, err := s.LoadInfo("test-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != info.ID || got.Title != info.Title || got.Archived != false {
		t.Errorf("got %+v, want %+v", got, info)
	}
}

func TestIndexLoadNotFound(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir, "/tmp")
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.LoadInfo("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestIndexListFiltersArchived(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir, "/tmp")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	s.SaveInfo(SessionInfo{ID: "a", Title: "Active", CreatedAt: now, LastActive: now})
	s.SaveInfo(SessionInfo{ID: "b", Title: "Archived", CreatedAt: now, LastActive: now, Archived: true})
	s.SaveInfo(SessionInfo{ID: "c", Title: "Also active", CreatedAt: now, LastActive: now})

	// Without archived
	active, err := s.ListInfo(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 {
		t.Errorf("expected 2 active sessions, got %d", len(active))
	}

	// With archived
	all, err := s.ListInfo(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 total sessions, got %d", len(all))
	}
}

func TestIndexLastEntryWins(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir, "/tmp")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	s.SaveInfo(SessionInfo{ID: "x", Title: "Original", CreatedAt: now, LastActive: now})
	s.SaveInfo(SessionInfo{ID: "x", Title: "Updated", CreatedAt: now, LastActive: now, Archived: true})

	got, err := s.LoadInfo("x")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Updated" || !got.Archived {
		t.Errorf("expected updated info, got %+v", got)
	}

	// Should not appear in active list
	active, _ := s.ListInfo(false)
	if len(active) != 0 {
		t.Errorf("expected 0 active sessions, got %d", len(active))
	}
}

func TestIndexPersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Truncate(time.Millisecond)

	// First instance writes
	s1, _ := NewFileStore(dir, "/tmp")
	s1.SaveInfo(SessionInfo{ID: "persist", Title: "Survives", CreatedAt: now, LastActive: now})

	// Second instance reads
	s2, _ := NewFileStore(dir, "/tmp")
	got, err := s2.LoadInfo("persist")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Survives" {
		t.Errorf("expected 'Survives', got %q", got.Title)
	}
}

func TestIndexFileCreatedOnDemand(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewFileStore(dir, "/tmp")

	// No file yet
	_, err := os.Stat(s.index.path)
	if !os.IsNotExist(err) {
		t.Error("index file should not exist before first save")
	}

	// After save, file exists
	s.SaveInfo(SessionInfo{ID: "first", Title: "First"})
	_, err = os.Stat(s.index.path)
	if err != nil {
		t.Error("index file should exist after save")
	}
}
