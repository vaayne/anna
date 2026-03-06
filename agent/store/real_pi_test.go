package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/vaayne/anna/agent/runner"
)

// TestLoadRealPiSessions loads actual Pi session files from ~/.pi/agent/sessions
// and verifies our store can parse them correctly.
// Skipped if no Pi sessions exist.
func TestLoadRealPiSessions(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot get home dir")
	}

	sessionsRoot := filepath.Join(homeDir, ".pi", "agent", "sessions", "--Users-weliu-workspace-anna--")
	if _, err := os.Stat(sessionsRoot); os.IsNotExist(err) {
		t.Skip("no Pi sessions directory found")
	}

	// Collect up to 5 jsonl files from any subdirectory.
	var files []string
	_ = filepath.Walk(sessionsRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if filepath.Ext(path) == ".jsonl" && info.Size() > 100 {
			files = append(files, path)
		}
		if len(files) >= 5 {
			return filepath.SkipAll
		}
		return nil
	})

	if len(files) == 0 {
		t.Skip("no Pi session files found")
	}

	// Use a temp store just for the Load method.
	dir := t.TempDir()
	s, err := NewFileStore(dir, "/tmp/test")
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			// Copy file into our store dir so Load can find it.
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			sessionID := "pi-" + filepath.Base(file[:len(file)-len(".jsonl")])
			dest := filepath.Join(dir, sessionID+".jsonl")
			if err := os.WriteFile(dest, data, 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			events, err := s.Load(sessionID)
			if err != nil {
				t.Fatalf("Load failed: %v", err)
			}

			t.Logf("Loaded %d events from %s (%d bytes)", len(events), filepath.Base(file), len(data))

			if len(events) == 0 {
				t.Fatal("expected at least some events")
			}

			// Verify basic structure.
			var userCount, assistantCount, toolCallCount, toolResultCount, otherCount int
			for _, evt := range events {
				switch evt.Type {
				case runner.RPCEventUserMessage:
					userCount++
					if evt.Summary == "" {
						t.Error("user message with empty summary")
					}
				case runner.RPCEventMessageUpdate:
					assistantCount++
				case runner.RPCEventToolCall:
					toolCallCount++
					if evt.Tool == "" {
						t.Error("tool call with empty tool name")
					}
					if evt.ID == "" {
						t.Error("tool call with empty ID")
					}
				case runner.RPCEventToolResult:
					toolResultCount++
					if evt.ID == "" {
						t.Error("tool result with empty ID")
					}
				default:
					otherCount++
				}
			}

			t.Logf("  user=%d assistant=%d toolCall=%d toolResult=%d other=%d",
				userCount, assistantCount, toolCallCount, toolResultCount, otherCount)

			if userCount == 0 {
				t.Error("expected at least one user message")
			}

			// Print first few events for inspection.
			for i, evt := range events {
				if i >= 3 {
					break
				}
				summary := evt.Summary
				if len(summary) > 80 {
					summary = summary[:80] + "..."
				}
				t.Logf("  [%d] type=%s tool=%s summary=%q", i, evt.Type, evt.Tool, summary)
			}

			// Verify tool calls have matching results (sanity check).
			callIDs := map[string]bool{}
			for _, evt := range events {
				if evt.Type == runner.RPCEventToolCall {
					callIDs[evt.ID] = true
				}
			}
			for _, evt := range events {
				if evt.Type == runner.RPCEventToolResult {
					if !callIDs[evt.ID] {
						t.Logf("  WARN: tool result %q has no matching call (may be from compacted history)", evt.ID)
					}
				}
			}

			fmt.Fprintf(os.Stderr, "✓ %s: %d events\n", filepath.Base(file), len(events))
		})
	}
}
