package memory

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// File identifies a persistent markdown file managed by the Store.
type File string

const (
	FileSoul File = "SOUL.md"
	FileUser File = "USER.md"
	FileFact File = "FACT.md"
)

// JournalEntry is a single entry in the append-only journal.
type JournalEntry struct {
	Timestamp time.Time `json:"ts"`
	Tags      []string  `json:"tags"`
	Text      string    `json:"text"`
}

// Store manages persistent memory: markdown files (soul, user, facts) and
// an append-only journal (JSONL, searchable via tool).
type Store struct {
	dir         string
	journalPath string
}

// NewStore creates a Store rooted at the given memory directory.
func NewStore(dir string) *Store {
	return &Store{
		dir:         dir,
		journalPath: filepath.Join(dir, "JOURNAL.jsonl"),
	}
}

// Read returns the current contents of the given file. Returns empty string
// if the file does not exist. File lookup is case-insensitive.
func (s *Store) Read(f File) (string, error) {
	path := s.resolve(f)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	return strings.TrimSpace(string(data)), err
}

// Write atomically overwrites the given file using its canonical name.
func (s *Store) Write(f File, content string) error {
	path := filepath.Join(s.dir, string(f))
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Dir returns the memory directory path.
func (s *Store) Dir() string {
	return s.dir
}

// Path returns the full path to the given file, resolving case-insensitively
// if an existing file matches. Falls back to the canonical name.
func (s *Store) Path(f File) string {
	return s.resolve(f)
}

// resolve finds the actual file path with case-insensitive matching.
// If no existing file matches, returns the canonical path.
func (s *Store) resolve(f File) string {
	canonical := filepath.Join(s.dir, string(f))
	if _, err := os.Stat(canonical); err == nil {
		return canonical
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return canonical
	}
	target := strings.ToLower(string(f))
	for _, e := range entries {
		if strings.ToLower(e.Name()) == target {
			return filepath.Join(s.dir, e.Name())
		}
	}
	return canonical
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
