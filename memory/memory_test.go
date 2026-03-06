package memory

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStore_Write_Read(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	// Read non-existent file returns empty.
	got, err := s.Read(FileFact)
	if err != nil {
		t.Fatalf("Read on empty: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}

	// Write and read back.
	want := "# Facts\n\n- User prefers Go."
	if err := s.Write(FileFact, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err = s.Read(FileFact)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// No tmp file left behind.
	if _, err := os.Stat(s.Path(FileFact) + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("tmp file should not exist after atomic write")
	}
}

func TestStore_Write_Atomic(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	original := "original content"
	if err := s.Write(FileFact, original); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Overwrite with new content.
	updated := "updated content"
	if err := s.Write(FileFact, updated); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, _ := s.Read(FileFact)
	if got != updated {
		t.Fatalf("got %q, want %q", got, updated)
	}
}

func TestStore_Append_Search(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	// Search empty journal returns nil.
	results, err := s.Search("", "", 10)
	if err != nil {
		t.Fatalf("Search empty: %v", err)
	}
	if results != nil {
		t.Fatalf("expected nil, got %v", results)
	}

	// Append entries.
	entries := []JournalEntry{
		{Timestamp: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC), Tags: []string{"deploy"}, Text: "Deployed v1 to staging"},
		{Timestamp: time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC), Tags: []string{"deploy", "prod"}, Text: "Deployed v1 to production"},
		{Timestamp: time.Date(2026, 1, 3, 10, 0, 0, 0, time.UTC), Tags: []string{"meeting"}, Text: "Sprint planning for Q2"},
	}
	for _, e := range entries {
		if err := s.Append(e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	// Search by query.
	results, err = s.Search("deployed", "", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// Results should be reverse-chronological.
	if results[0].Text != "Deployed v1 to production" {
		t.Fatalf("expected production first, got %q", results[0].Text)
	}

	// Search by tag.
	results, err = s.Search("", "prod", 10)
	if err != nil {
		t.Fatalf("Search by tag: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	// Search with limit.
	results, err = s.Search("", "", 2)
	if err != nil {
		t.Fatalf("Search with limit: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// Should be the 2 most recent.
	if results[0].Text != "Sprint planning for Q2" {
		t.Fatalf("expected sprint planning first, got %q", results[0].Text)
	}
}

func TestStore_Search_CaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	_ = s.Append(JournalEntry{Timestamp: time.Now(), Text: "Fixed Bug in AuthModule"})

	results, _ := s.Search("fixed bug", "", 10)
	if len(results) != 1 {
		t.Fatalf("expected case-insensitive match, got %d results", len(results))
	}
}

func TestStore_Search_TagCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	_ = s.Append(JournalEntry{Timestamp: time.Now(), Tags: []string{"Deploy"}, Text: "deployed"})

	results, _ := s.Search("", "deploy", 10)
	if len(results) != 1 {
		t.Fatalf("expected case-insensitive tag match, got %d results", len(results))
	}
}

func TestStore_JournalFileCreated(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	_ = s.Append(JournalEntry{Timestamp: time.Now(), Text: "test"})

	if _, err := os.Stat(filepath.Join(dir, "JOURNAL.jsonl")); err != nil {
		t.Fatalf("journal file should exist: %v", err)
	}
}
