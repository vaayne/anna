package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vaayne/anna/agent/runner"
)

func tempStore(t *testing.T) *FileStore {
	t.Helper()
	dir := t.TempDir()
	s, err := NewFileStore(dir, "/tmp/test")
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	return s
}

func TestAppendAndLoad(t *testing.T) {
	s := tempStore(t)

	events := []runner.RPCEvent{
		{Type: "user_message", Summary: "hello"},
		{Type: "message_update", Summary: "hi there"},
	}

	if err := s.Append("s1", events...); err != nil {
		t.Fatalf("Append: %v", err)
	}

	loaded, err := s.Load("s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected 2 events, got %d", len(loaded))
	}
	if loaded[0].Type != "user_message" || loaded[0].Summary != "hello" {
		t.Errorf("event 0 mismatch: %+v", loaded[0])
	}
	if loaded[1].Type != "message_update" || loaded[1].Summary != "hi there" {
		t.Errorf("event 1 mismatch: %+v", loaded[1])
	}
}

func TestAppendIsIncremental(t *testing.T) {
	s := tempStore(t)

	if err := s.Append("s1", runner.RPCEvent{Type: "user_message", Summary: "first"}); err != nil {
		t.Fatalf("Append 1: %v", err)
	}
	if err := s.Append("s1", runner.RPCEvent{Type: "user_message", Summary: "second"}); err != nil {
		t.Fatalf("Append 2: %v", err)
	}

	loaded, err := s.Load("s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected 2 events, got %d", len(loaded))
	}
	if loaded[1].Summary != "second" {
		t.Errorf("expected 'second', got %q", loaded[1].Summary)
	}
}

func TestLoadNonexistent(t *testing.T) {
	s := tempStore(t)

	events, err := s.Load("nope")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if events != nil {
		t.Fatalf("expected nil, got %v", events)
	}
}

func TestDelete(t *testing.T) {
	s := tempStore(t)

	if err := s.Append("s1", runner.RPCEvent{Type: "user_message", Summary: "hello"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Delete("s1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	events, err := s.Load("s1")
	if err != nil {
		t.Fatalf("Load after delete: %v", err)
	}
	if events != nil {
		t.Fatalf("expected nil after delete, got %v", events)
	}
}

func TestDeleteNonexistent(t *testing.T) {
	s := tempStore(t)
	if err := s.Delete("nope"); err != nil {
		t.Fatalf("Delete nonexistent: %v", err)
	}
}

func TestList(t *testing.T) {
	s := tempStore(t)

	if err := s.Append("alpha", runner.RPCEvent{Type: "user_message"}); err != nil {
		t.Fatalf("Append alpha: %v", err)
	}
	if err := s.Append("beta", runner.RPCEvent{Type: "user_message"}); err != nil {
		t.Fatalf("Append beta: %v", err)
	}

	ids, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(ids))
	}

	found := map[string]bool{}
	for _, id := range ids {
		found[id] = true
	}
	if !found["alpha"] || !found["beta"] {
		t.Errorf("expected alpha and beta, got %v", ids)
	}
}

func TestPathSanitization(t *testing.T) {
	s := tempStore(t)

	if err := s.Append("../evil", runner.RPCEvent{Type: "user_message"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Should NOT create a file outside the store dir.
	if _, err := os.Stat(filepath.Join(s.dir, "..", "evil.jsonl")); !os.IsNotExist(err) {
		t.Fatal("path traversal was not prevented")
	}

	// Should exist inside the store dir.
	events, err := s.Load("../evil")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
}

func TestTimestampedFilename(t *testing.T) {
	s := tempStore(t)

	if err := s.Append("my-session", runner.RPCEvent{Type: "user_message", Summary: "hello"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// File should be created with timestamp prefix.
	entries, _ := os.ReadDir(s.dir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}
	name := entries[0].Name()
	if !strings.HasSuffix(name, "_my-session.jsonl") {
		t.Errorf("expected timestamped filename, got %q", name)
	}
	if !strings.Contains(name, "T") {
		t.Errorf("expected ISO timestamp in filename, got %q", name)
	}

	// resolve should find it.
	p := s.resolve("my-session")
	if p == "" {
		t.Fatal("resolve failed to find timestamped file")
	}
}

func TestPiFormatSessionHeader(t *testing.T) {
	s := tempStore(t)

	if err := s.Append("test-session", runner.RPCEvent{Type: "user_message", Summary: "hi"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Read raw file and verify Pi format.
	p := s.resolve("test-session")
	if p == "" {
		t.Fatal("session file not found")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines, got %d", len(lines))
	}

	// Verify header.
	var header sessionHeader
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header.Type != "session" {
		t.Errorf("header type: expected 'session', got %q", header.Type)
	}
	if header.Version != 3 {
		t.Errorf("header version: expected 3, got %d", header.Version)
	}

	// Verify message entry.
	var entry sessionEntry
	if err := json.Unmarshal([]byte(lines[1]), &entry); err != nil {
		t.Fatalf("unmarshal entry: %v", err)
	}
	if entry.Type != "message" {
		t.Errorf("entry type: expected 'message', got %q", entry.Type)
	}
	if entry.ID == "" {
		t.Error("entry ID should not be empty")
	}

	// Verify it's a user message in Pi format (content block array).
	var msg piUserMessage
	if err := json.Unmarshal(entry.Message, &msg); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	if msg.Role != "user" {
		t.Errorf("message role: expected 'user', got %q", msg.Role)
	}
	// Verify content is written as content block array (issue #5).
	var blocks []piTextContent
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		t.Fatalf("user content should be array: %v", err)
	}
	if len(blocks) != 1 || blocks[0].Text != "hi" {
		t.Errorf("user content blocks: %+v", blocks)
	}
}

func TestPiFormatToolCallRoundTrip(t *testing.T) {
	s := tempStore(t)

	argsJSON, _ := json.Marshal(map[string]any{"command": "ls -la"})
	events := []runner.RPCEvent{
		{Type: "user_message", Summary: "list files"},
		{
			Type:   runner.RPCEventToolCall,
			ID:     "call-123",
			Tool:   "bash",
			Result: argsJSON,
		},
		{
			Type:   runner.RPCEventToolResult,
			ID:     "call-123",
			Tool:   "bash",
			Result: json.RawMessage(`"file1.txt\nfile2.txt"`),
		},
		{Type: "message_update", Summary: "Here are your files."},
	}

	if err := s.Append("s1", events...); err != nil {
		t.Fatalf("Append: %v", err)
	}

	loaded, err := s.Load("s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded) != 4 {
		t.Fatalf("expected 4 events, got %d", len(loaded))
	}

	// User message.
	if loaded[0].Type != runner.RPCEventUserMessage || loaded[0].Summary != "list files" {
		t.Errorf("event 0: %+v", loaded[0])
	}

	// Tool call.
	if loaded[1].Type != runner.RPCEventToolCall || loaded[1].Tool != "bash" || loaded[1].ID != "call-123" {
		t.Errorf("event 1: %+v", loaded[1])
	}
	var args map[string]any
	if err := json.Unmarshal(loaded[1].Result, &args); err != nil {
		t.Errorf("event 1 args: %v", err)
	}
	if args["command"] != "ls -la" {
		t.Errorf("event 1 args: expected 'ls -la', got %v", args["command"])
	}

	// Tool result.
	if loaded[2].Type != runner.RPCEventToolResult || loaded[2].Tool != "bash" {
		t.Errorf("event 2: %+v", loaded[2])
	}

	// Assistant message.
	if loaded[3].Type != runner.RPCEventMessageUpdate || loaded[3].Summary != "Here are your files." {
		t.Errorf("event 3: %+v", loaded[3])
	}
}

func TestPiFormatParentChain(t *testing.T) {
	s := tempStore(t)

	events := []runner.RPCEvent{
		{Type: "user_message", Summary: "first"},
		{Type: "message_update", Summary: "response 1"},
		{Type: "user_message", Summary: "second"},
		{Type: "message_update", Summary: "response 2"},
	}

	if err := s.Append("s1", events...); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Read raw file and verify parentId chain.
	p := s.resolve("s1")
	if p == "" {
		t.Fatal("session file not found")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	// line 0 = header, lines 1-4 = entries
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines, got %d", len(lines))
	}

	var entries []sessionEntry
	for _, line := range lines[1:] {
		var e sessionEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		entries = append(entries, e)
	}

	// First entry should have null parentId.
	if entries[0].ParentID != nil {
		t.Errorf("first entry parentId: expected nil, got %v", *entries[0].ParentID)
	}

	// Each subsequent entry should reference the previous.
	for i := 1; i < len(entries); i++ {
		if entries[i].ParentID == nil || *entries[i].ParentID != entries[i-1].ID {
			t.Errorf("entry %d parentId: expected %q, got %v", i, entries[i-1].ID, entries[i].ParentID)
		}
	}
}

func TestLoadPiSessionFile(t *testing.T) {
	// Simulate loading a real Pi session file.
	dir := t.TempDir()
	s, _ := NewFileStore(dir, "/tmp/test")

	content := `{"type":"session","version":3,"id":"test-uuid","timestamp":"2026-03-06T09:24:57.725Z","cwd":"/tmp/test"}
{"type":"thinking_level_change","id":"aaa","parentId":null,"timestamp":"2026-03-06T09:24:57.725Z","thinkingLevel":"medium"}
{"type":"message","id":"bbb","parentId":"aaa","timestamp":"2026-03-06T09:24:58.467Z","message":{"role":"user","content":[{"type":"text","text":"hello world"}],"timestamp":1772789098465}}
{"type":"message","id":"ccc","parentId":"bbb","timestamp":"2026-03-06T09:25:02.440Z","message":{"role":"assistant","content":[{"type":"text","text":"Hi! How can I help?"},{"type":"toolCall","id":"tool1","name":"bash","arguments":{"command":"ls"}}],"api":"bedrock","provider":"amazon-bedrock","model":"claude","usage":{},"stopReason":"toolUse","timestamp":1772789098466}}
{"type":"message","id":"ddd","parentId":"ccc","timestamp":"2026-03-06T09:25:02.479Z","message":{"role":"toolResult","toolCallId":"tool1","toolName":"bash","content":[{"type":"text","text":"file1.txt\n"}],"isError":false,"timestamp":1772789102478}}
{"type":"message","id":"eee","parentId":"ddd","timestamp":"2026-03-06T09:25:05.000Z","message":{"role":"assistant","content":[{"type":"text","text":"I found file1.txt."}],"api":"bedrock","provider":"amazon-bedrock","model":"claude","usage":{},"stopReason":"stop","timestamp":1772789105000}}
`

	if err := os.WriteFile(filepath.Join(dir, "pi-session.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	events, err := s.Load("pi-session")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Should skip session header and thinking_level_change, load 5 messages.
	// The mixed text+toolCall assistant message produces 2 events (text + tool call).
	if len(events) != 5 {
		t.Fatalf("expected 5 events, got %d", len(events))
	}

	// User message (from content block array).
	if events[0].Type != runner.RPCEventUserMessage || events[0].Summary != "hello world" {
		t.Errorf("event 0: %+v", events[0])
	}

	// Assistant text from mixed message.
	if events[1].Type != runner.RPCEventMessageUpdate || events[1].Summary != "Hi! How can I help?" {
		t.Errorf("event 1: %+v", events[1])
	}

	// Tool call from mixed message.
	if events[2].Type != runner.RPCEventToolCall || events[2].Tool != "bash" || events[2].ID != "tool1" {
		t.Errorf("event 2: %+v", events[2])
	}

	// Tool result.
	if events[3].Type != runner.RPCEventToolResult || events[3].Tool != "bash" {
		t.Errorf("event 3: %+v", events[3])
	}

	// Final assistant text.
	if events[4].Type != runner.RPCEventMessageUpdate || events[4].Summary != "I found file1.txt." {
		t.Errorf("event 4: %+v", events[4])
	}
}

func TestLoadCompaction(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewFileStore(dir, "/tmp/test")

	content := `{"type":"session","version":3,"id":"compact-test","timestamp":"2026-03-06T09:24:57.725Z","cwd":"/tmp/test"}
{"type":"compaction","id":"aaa","parentId":null,"timestamp":"2026-03-06T09:24:57.725Z","summary":"The user asked about Go testing. We discussed table-driven tests and benchmarks."}
{"type":"message","id":"bbb","parentId":"aaa","timestamp":"2026-03-06T09:25:00.000Z","message":{"role":"user","content":[{"type":"text","text":"what about fuzzing?"}],"timestamp":1772789100000}}
{"type":"message","id":"ccc","parentId":"bbb","timestamp":"2026-03-06T09:25:05.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Go supports fuzzing natively since 1.18."}],"stopReason":"stop","timestamp":1772789105000}}
`

	if err := os.WriteFile(filepath.Join(dir, "compact-test.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	events, err := s.Load("compact-test")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Compaction produces 2 synthetic events + 2 real messages = 4 total.
	if len(events) != 4 {
		t.Fatalf("expected 4 events, got %d", len(events))
	}

	// Synthetic user from compaction.
	if events[0].Type != runner.RPCEventUserMessage || events[0].Summary != "[Previous conversation summary]" {
		t.Errorf("event 0: %+v", events[0])
	}

	// Compaction summary as assistant message.
	if events[1].Type != runner.RPCEventMessageUpdate || !strings.Contains(events[1].Summary, "Go testing") {
		t.Errorf("event 1: %+v", events[1])
	}

	// Real user message.
	if events[2].Type != runner.RPCEventUserMessage || events[2].Summary != "what about fuzzing?" {
		t.Errorf("event 2: %+v", events[2])
	}

	// Real assistant message.
	if events[3].Type != runner.RPCEventMessageUpdate || !strings.Contains(events[3].Summary, "fuzzing") {
		t.Errorf("event 3: %+v", events[3])
	}
}

func TestListTimestampedFiles(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewFileStore(dir, "/tmp/test")

	// Create timestamped files.
	if err := s.Append("alpha", runner.RPCEvent{Type: "user_message", Summary: "a"}); err != nil {
		t.Fatalf("Append alpha: %v", err)
	}
	if err := s.Append("beta", runner.RPCEvent{Type: "user_message", Summary: "b"}); err != nil {
		t.Fatalf("Append beta: %v", err)
	}

	ids, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2, got %d", len(ids))
	}

	found := map[string]bool{}
	for _, id := range ids {
		found[id] = true
	}
	if !found["alpha"] || !found["beta"] {
		t.Errorf("expected alpha and beta, got %v", ids)
	}
}

func TestEstimateTokens(t *testing.T) {
	s := tempStore(t)

	// No file → 0.
	tokens, err := s.EstimateTokens("nope")
	if err != nil {
		t.Fatalf("EstimateTokens: %v", err)
	}
	if tokens != 0 {
		t.Fatalf("expected 0, got %d", tokens)
	}

	// Append some events.
	events := []runner.RPCEvent{
		{Type: "user_message", Summary: "hello"},
		{Type: "message_update", Summary: "hi there"},
		{Type: "user_message", Summary: "how are you"},
		{Type: "message_update", Summary: "good"},
	}
	if err := s.Append("s1", events...); err != nil {
		t.Fatalf("Append: %v", err)
	}

	tokens, err = s.EstimateTokens("s1")
	if err != nil {
		t.Fatalf("EstimateTokens: %v", err)
	}
	// Should be > 0 (exact value depends on JSON encoding overhead).
	if tokens == 0 {
		t.Fatal("expected non-zero token estimate")
	}
}

func TestCompact(t *testing.T) {
	s := tempStore(t)

	// Build a conversation with 10 messages.
	for i := 0; i < 5; i++ {
		events := []runner.RPCEvent{
			{Type: "user_message", Summary: fmt.Sprintf("question %d", i)},
			{Type: "message_update", Summary: fmt.Sprintf("answer %d", i)},
		}
		if err := s.Append("s1", events...); err != nil {
			t.Fatalf("Append round %d: %v", i, err)
		}
	}

	// Verify 10 events before compaction.
	before, err := s.Load("s1")
	if err != nil {
		t.Fatalf("Load before: %v", err)
	}
	if len(before) != 10 {
		t.Fatalf("expected 10 events before compaction, got %d", len(before))
	}

	// Compact keeping last 4 message lines (2 user + 2 assistant).
	summary := "We discussed questions 0-4. All were answered."
	after, err := s.Compact("s1", summary, 4)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// Compaction produces: 2 synthetic (from compaction) + events from 4 kept lines.
	// Each kept message line produces 1 RPCEvent, so 2 + 4 = 6.
	if len(after) != 6 {
		t.Fatalf("expected 6 events after compaction, got %d", len(after))
	}

	// First two events are from compaction.
	if after[0].Type != runner.RPCEventUserMessage || after[0].Summary != "[Previous conversation summary]" {
		t.Errorf("event 0: %+v", after[0])
	}
	if after[1].Type != runner.RPCEventMessageUpdate || !strings.Contains(after[1].Summary, "questions 0-4") {
		t.Errorf("event 1: %+v", after[1])
	}

	// Kept events should be the last 4 messages (question 3, answer 3, question 4, answer 4).
	if after[2].Summary != "question 3" {
		t.Errorf("expected 'question 3', got %q", after[2].Summary)
	}
	if after[5].Summary != "answer 4" {
		t.Errorf("expected 'answer 4', got %q", after[5].Summary)
	}

	// Token estimate should be smaller after compaction.
	tokens, err := s.EstimateTokens("s1")
	if err != nil {
		t.Fatalf("EstimateTokens after compact: %v", err)
	}
	if tokens == 0 {
		t.Fatal("expected non-zero token estimate after compaction")
	}

	// Appending after compaction should still work.
	if err := s.Append("s1", runner.RPCEvent{Type: "user_message", Summary: "follow-up"}); err != nil {
		t.Fatalf("Append after compact: %v", err)
	}
	final, err := s.Load("s1")
	if err != nil {
		t.Fatalf("Load final: %v", err)
	}
	if len(final) != 7 {
		t.Fatalf("expected 7 events after append, got %d", len(final))
	}
	if final[6].Summary != "follow-up" {
		t.Errorf("expected 'follow-up', got %q", final[6].Summary)
	}
}

func TestCompactNonexistent(t *testing.T) {
	s := tempStore(t)
	_, err := s.Compact("nope", "summary", 5)
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestCompactKeepAll(t *testing.T) {
	s := tempStore(t)

	events := []runner.RPCEvent{
		{Type: "user_message", Summary: "q1"},
		{Type: "message_update", Summary: "a1"},
	}
	if err := s.Append("s1", events...); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// keepTail >= message count → keeps everything.
	after, err := s.Compact("s1", "summary", 100)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	// 2 synthetic + 2 kept = 4
	if len(after) != 4 {
		t.Fatalf("expected 4 events, got %d", len(after))
	}
}
