package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/vaayne/anna/agent/runner"
)

// Store persists session event history across restarts.
type Store interface {
	// Append writes events to the session log.
	Append(sessionID string, events ...runner.RPCEvent) error
	// Load reads the full event history for a session.
	Load(sessionID string) ([]runner.RPCEvent, error)
	// Delete removes a session's persisted history.
	Delete(sessionID string) error
	// List returns all known session IDs.
	List() ([]string, error)
}

// Pi session file format (JSONL, compatible with pi-mono SessionManager).
//
// Line 1: SessionHeader
//   {"type":"session","version":3,"id":"...","timestamp":"...","cwd":"..."}
//
// Subsequent lines: SessionEntry (we only use "message" type)
//   {"type":"message","id":"...","parentId":"...","timestamp":"...","message":{...}}
//
// Message payloads follow pi-mono's Message types:
//   UserMessage:       {"role":"user","content":"...","timestamp":1234567890}
//   AssistantMessage:  {"role":"assistant","content":[{"type":"text","text":"..."}],...,"timestamp":1234567890}
//   ToolResultMessage: {"role":"toolResult","toolCallId":"...","toolName":"...","content":[...],"isError":false,"timestamp":1234567890}

const currentSessionVersion = 3

// sessionHeader is the first line in a Pi session JSONL file.
type sessionHeader struct {
	Type      string `json:"type"`
	Version   int    `json:"version"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
}

// sessionEntry is a single entry in a Pi session JSONL file.
type sessionEntry struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ParentID  *string         `json:"parentId"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message,omitempty"`
}

// piMessage types that match pi-mono's format.
type piUserMessage struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Timestamp int64           `json:"timestamp"`
}

type piTextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type piToolCall struct {
	Type      string         `json:"type"`
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type piAssistantMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Api        string          `json:"api,omitempty"`
	Provider   string          `json:"provider,omitempty"`
	Model      string          `json:"model,omitempty"`
	Usage      json.RawMessage `json:"usage,omitempty"`
	StopReason string          `json:"stopReason,omitempty"`
	Timestamp  int64           `json:"timestamp"`
}

type piToolResultMessage struct {
	Role       string          `json:"role"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Content    json.RawMessage `json:"content"`
	IsError    bool            `json:"isError"`
	Timestamp  int64           `json:"timestamp"`
}

// FileStore persists sessions as Pi-compatible JSONL files on disk.
type FileStore struct {
	dir string
	cwd string
	// lastParentID tracks the last entry ID per session for parentId chaining.
	lastParentID map[string]string
}

// NewFileStore creates a FileStore rooted at dir.
// The directory is created if it does not exist.
func NewFileStore(dir string, cwd string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}
	return &FileStore{dir: dir, cwd: cwd, lastParentID: make(map[string]string)}, nil
}

func (s *FileStore) path(sessionID string) string {
	// Sanitize to prevent path traversal.
	safe := strings.ReplaceAll(sessionID, string(os.PathSeparator), "_")
	return filepath.Join(s.dir, safe+".jsonl")
}

func shortID() string {
	return uuid.New().String()[:8]
}

// ensureHeader creates the session file with a header if it doesn't exist.
func (s *FileStore) ensureHeader(sessionID string) error {
	p := s.path(sessionID)
	if _, err := os.Stat(p); err == nil {
		return nil // file exists
	}
	header := sessionHeader{
		Type:      "session",
		Version:   currentSessionVersion,
		ID:        sessionID,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Cwd:       s.cwd,
	}
	data, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("marshal header: %w", err)
	}
	return os.WriteFile(p, append(data, '\n'), 0o644)
}

// Append appends events to the session file in Pi JSONL format.
func (s *FileStore) Append(sessionID string, events ...runner.RPCEvent) error {
	if err := s.ensureHeader(sessionID); err != nil {
		return err
	}

	f, err := os.OpenFile(s.path(sessionID), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open session file: %w", err)
	}
	defer f.Close()

	for _, evt := range events {
		entry, ok := rpcEventToEntry(evt, s.lastParentID[sessionID])
		if !ok {
			continue
		}
		s.lastParentID[sessionID] = entry.ID
		data, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("marshal entry: %w", err)
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			return fmt.Errorf("write entry: %w", err)
		}
	}
	return nil
}

// Load reads all events from the session file and converts to RPCEvents.
// Returns nil, nil if the session does not exist.
func (s *FileStore) Load(sessionID string) ([]runner.RPCEvent, error) {
	f, err := os.Open(s.path(sessionID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open session file: %w", err)
	}
	defer f.Close()

	var events []runner.RPCEvent
	var lastEntryID string

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Peek at type to decide how to parse.
		var peek struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &peek); err != nil {
			continue
		}

		// Skip non-message entries (session header, thinking_level_change, model_change, etc.)
		if peek.Type != "message" {
			continue
		}

		var entry sessionEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}

		lastEntryID = entry.ID

		evt, ok := entryToRPCEvent(entry)
		if ok {
			events = append(events, evt)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read session file: %w", err)
	}

	// Restore parentID chain for future appends.
	if lastEntryID != "" {
		s.lastParentID[sessionID] = lastEntryID
	}

	return events, nil
}

// Delete removes the session file.
func (s *FileStore) Delete(sessionID string) error {
	err := os.Remove(s.path(sessionID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// List returns session IDs for all stored sessions.
func (s *FileStore) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("read session dir: %w", err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".jsonl") {
			ids = append(ids, strings.TrimSuffix(name, ".jsonl"))
		}
	}
	return ids, nil
}

// rpcEventToEntry converts an RPCEvent to a Pi-compatible sessionEntry.
func rpcEventToEntry(evt runner.RPCEvent, parentID string) (sessionEntry, bool) {
	now := time.Now()
	ts := now.UTC().Format(time.RFC3339Nano)
	tsMs := now.UnixMilli()

	entry := sessionEntry{
		Type:      "message",
		ID:        shortID(),
		Timestamp: ts,
	}
	if parentID != "" {
		entry.ParentID = &parentID
	}

	switch evt.Type {
	case runner.RPCEventUserMessage:
		content := evt.Summary
		contentJSON, _ := json.Marshal(content)
		msg := piUserMessage{
			Role:      "user",
			Content:   contentJSON,
			Timestamp: tsMs,
		}
		entry.Message, _ = json.Marshal(msg)
		return entry, true

	case runner.RPCEventMessageUpdate:
		// Complete assistant message (persisted with Summary).
		text := evt.Summary
		if text == "" {
			return entry, false
		}
		contentJSON, _ := json.Marshal([]piTextContent{{Type: "text", Text: text}})
		msg := piAssistantMessage{
			Role:       "assistant",
			Content:    contentJSON,
			StopReason: "stop",
			Timestamp:  tsMs,
		}
		entry.Message, _ = json.Marshal(msg)
		return entry, true

	case runner.RPCEventToolCall:
		var args map[string]any
		if len(evt.Result) > 0 {
			_ = json.Unmarshal(evt.Result, &args)
		}
		// Tool calls are part of assistant message content in Pi format.
		// We store them as a standalone assistant message with a single toolCall block.
		tc := piToolCall{
			Type:      "toolCall",
			ID:        evt.ID,
			Name:      evt.Tool,
			Arguments: args,
		}
		contentJSON, _ := json.Marshal([]any{tc})
		msg := piAssistantMessage{
			Role:       "assistant",
			Content:    contentJSON,
			StopReason: "toolUse",
			Timestamp:  tsMs,
		}
		entry.Message, _ = json.Marshal(msg)
		return entry, true

	case runner.RPCEventToolResult:
		var resultText string
		if len(evt.Result) > 0 {
			_ = json.Unmarshal(evt.Result, &resultText)
		}
		contentJSON, _ := json.Marshal([]piTextContent{{Type: "text", Text: resultText}})
		msg := piToolResultMessage{
			Role:       "toolResult",
			ToolCallID: evt.ID,
			ToolName:   evt.Tool,
			Content:    contentJSON,
			IsError:    evt.Error != "",
			Timestamp:  tsMs,
		}
		entry.Message, _ = json.Marshal(msg)
		return entry, true

	default:
		// Skip tool_start, tool_end, agent_end — these are transient events.
		return entry, false
	}
}

// entryToRPCEvent converts a Pi sessionEntry back to an RPCEvent.
func entryToRPCEvent(entry sessionEntry) (runner.RPCEvent, bool) {
	if entry.Type != "message" || len(entry.Message) == 0 {
		return runner.RPCEvent{}, false
	}

	var peek struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(entry.Message, &peek); err != nil {
		return runner.RPCEvent{}, false
	}

	switch peek.Role {
	case "user":
		var msg piUserMessage
		if err := json.Unmarshal(entry.Message, &msg); err != nil {
			return runner.RPCEvent{}, false
		}
		// Content can be string or array of content blocks.
		text := extractUserText(msg.Content)
		return runner.RPCEvent{
			Type:    runner.RPCEventUserMessage,
			Summary: text,
		}, true

	case "assistant":
		var msg piAssistantMessage
		if err := json.Unmarshal(entry.Message, &msg); err != nil {
			return runner.RPCEvent{}, false
		}
		// Parse content blocks to find text and tool calls.
		text, toolCalls := parseAssistantContent(msg.Content)

		// If there are tool calls, emit them as separate events? No — return the
		// first tool call as a tool_call event, or text as message_update.
		// For simplicity: if it has text, return as message_update; tool calls
		// will be loaded as tool_call events.
		if len(toolCalls) > 0 {
			tc := toolCalls[0]
			argsJSON, _ := json.Marshal(tc.Arguments)
			return runner.RPCEvent{
				Type:   runner.RPCEventToolCall,
				ID:     tc.ID,
				Tool:   tc.Name,
				Result: argsJSON,
			}, true
		}

		return runner.RPCEvent{
			Type:    runner.RPCEventMessageUpdate,
			Summary: text,
		}, true

	case "toolResult":
		var msg piToolResultMessage
		if err := json.Unmarshal(entry.Message, &msg); err != nil {
			return runner.RPCEvent{}, false
		}
		resultText := extractTextFromContent(msg.Content)
		contentJSON, _ := json.Marshal(resultText)
		evt := runner.RPCEvent{
			Type:   runner.RPCEventToolResult,
			ID:     msg.ToolCallID,
			Tool:   msg.ToolName,
			Result: contentJSON,
		}
		if msg.IsError {
			evt.Error = resultText
		}
		return evt, true

	default:
		return runner.RPCEvent{}, false
	}
}

// extractUserText extracts text from Pi user message content (string or content block array).
func extractUserText(raw json.RawMessage) string {
	// Try string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Try array of content blocks.
	return extractTextFromContent(raw)
}

// extractTextFromContent extracts concatenated text from a JSON array of content blocks.
func extractTextFromContent(raw json.RawMessage) string {
	var blocks []piTextContent
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "")
}

// parseAssistantContent parses assistant content blocks into text and tool calls.
func parseAssistantContent(raw json.RawMessage) (string, []piToolCall) {
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", nil
	}

	var text string
	var toolCalls []piToolCall

	for _, block := range blocks {
		var peek struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(block, &peek); err != nil {
			continue
		}
		switch peek.Type {
		case "text":
			var tc piTextContent
			if json.Unmarshal(block, &tc) == nil {
				text += tc.Text
			}
		case "toolCall":
			var call piToolCall
			if json.Unmarshal(block, &call) == nil {
				toolCalls = append(toolCalls, call)
			}
		}
	}
	return text, toolCalls
}
