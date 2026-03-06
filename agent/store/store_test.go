package store

import (
	"encoding/json"
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

func TestPiFormatSessionHeader(t *testing.T) {
	s := tempStore(t)

	if err := s.Append("test-session", runner.RPCEvent{Type: "user_message", Summary: "hi"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Read raw file and verify Pi format.
	data, err := os.ReadFile(s.path("test-session"))
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

	// Verify it's a user message in Pi format.
	var msg piUserMessage
	if err := json.Unmarshal(entry.Message, &msg); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	if msg.Role != "user" {
		t.Errorf("message role: expected 'user', got %q", msg.Role)
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
	data, err := os.ReadFile(s.path("s1"))
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

	// Should skip session header and thinking_level_change, load 4 messages.
	if len(events) != 4 {
		t.Fatalf("expected 4 events, got %d", len(events))
	}

	// User message (from content block array).
	if events[0].Type != runner.RPCEventUserMessage || events[0].Summary != "hello world" {
		t.Errorf("event 0: %+v", events[0])
	}

	// Assistant with tool call — should load as tool_call since it has toolCall content.
	if events[1].Type != runner.RPCEventToolCall || events[1].Tool != "bash" || events[1].ID != "tool1" {
		t.Errorf("event 1: %+v", events[1])
	}

	// Tool result.
	if events[2].Type != runner.RPCEventToolResult || events[2].Tool != "bash" {
		t.Errorf("event 2: %+v", events[2])
	}

	// Final assistant text.
	if events[3].Type != runner.RPCEventMessageUpdate || events[3].Summary != "I found file1.txt." {
		t.Errorf("event 3: %+v", events[3])
	}
}
