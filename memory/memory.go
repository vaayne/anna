package memory

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// JournalEntry is a single entry in the append-only journal.
type JournalEntry struct {
	Timestamp time.Time `json:"ts"`
	Tags      []string  `json:"tags"`
	Text      string    `json:"text"`
}

// Store manages persistent memory: a facts file (markdown, always in system
// prompt) and a journal (JSONL, searchable via tool).
type Store struct {
	factsPath   string // .agents/memory.md
	journalPath string // .agents/journal.jsonl
}

// NewStore creates a Store rooted at the given agents directory.
func NewStore(agentsDir string) *Store {
	return &Store{
		factsPath:   filepath.Join(agentsDir, "memory.md"),
		journalPath: filepath.Join(agentsDir, "journal.jsonl"),
	}
}

// ReadFacts returns the current contents of memory.md.
func (s *Store) ReadFacts() (string, error) {
	data, err := os.ReadFile(s.factsPath)
	if os.IsNotExist(err) {
		return "", nil
	}
	return string(data), err
}

// WriteFacts atomically overwrites memory.md.
func (s *Store) WriteFacts(content string) error {
	if err := os.MkdirAll(filepath.Dir(s.factsPath), 0o755); err != nil {
		return err
	}
	tmp := s.factsPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.factsPath)
}

// Append adds an entry to the journal.
func (s *Store) Append(entry JournalEntry) error {
	if err := os.MkdirAll(filepath.Dir(s.journalPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.journalPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// Search scans the journal for entries matching a query string and/or tag.
// Results are returned in reverse chronological order, up to limit.
func (s *Store) Search(query, tag string, limit int) ([]JournalEntry, error) {
	if limit <= 0 {
		limit = 20
	}

	f, err := os.Open(s.journalPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Read all matching entries, then take the last N.
	var matches []JournalEntry
	queryLower := strings.ToLower(query)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var entry JournalEntry
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if tag != "" && !hasTag(entry.Tags, tag) {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(entry.Text), queryLower) {
			continue
		}
		matches = append(matches, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// Return last `limit` entries (most recent).
	if len(matches) > limit {
		matches = matches[len(matches)-limit:]
	}
	// Reverse for reverse-chronological order.
	for i, j := 0, len(matches)-1; i < j; i, j = i+1, j-1 {
		matches[i], matches[j] = matches[j], matches[i]
	}
	return matches, nil
}

func hasTag(tags []string, target string) bool {
	target = strings.ToLower(target)
	for _, t := range tags {
		if strings.ToLower(t) == target {
			return true
		}
	}
	return false
}
