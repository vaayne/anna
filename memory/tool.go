package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

var memoryInputSchema = func() map[string]any {
	var m map[string]any
	_ = json.Unmarshal([]byte(`{
  "type": "object",
  "properties": {
    "action": {
      "type": "string",
      "enum": ["update", "append", "search"],
      "description": "Action to perform: 'update' overwrites facts (memory.md), 'append' adds a journal entry, 'search' queries the journal"
    },
    "content": {
      "type": "string",
      "description": "Full markdown content for memory.md (required for update)"
    },
    "text": {
      "type": "string",
      "description": "Journal entry text (required for append)"
    },
    "tags": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Tags for the journal entry (optional, for append)"
    },
    "query": {
      "type": "string",
      "description": "Search query — case-insensitive substring match (for search)"
    },
    "tag": {
      "type": "string",
      "description": "Filter by tag (optional, for search)"
    },
    "limit": {
      "type": "integer",
      "description": "Max results to return (default 20, for search)"
    }
  },
  "required": ["action"]
}`), &m)
	return m
}()

// MemoryTool exposes memory management as an agent tool.
type MemoryTool struct {
	store *Store
}

// NewTool creates a MemoryTool backed by the given store.
func NewTool(store *Store) *MemoryTool {
	return &MemoryTool{store: store}
}

// Definition returns the tool definition for the LLM.
func (t *MemoryTool) Definition() aitypes.ToolDefinition {
	return aitypes.ToolDefinition{
		Name:        "memory",
		Description: "Manage persistent memory across sessions. Use 'update' to overwrite facts (memory.md — always in your system prompt), 'append' to log events to the journal, or 'search' to query past journal entries.",
		InputSchema: memoryInputSchema,
	}
}

// Execute runs the memory tool action.
func (t *MemoryTool) Execute(_ context.Context, args map[string]any) (string, error) {
	action, _ := args["action"].(string)
	switch action {
	case "update":
		return t.update(args)
	case "append":
		return t.appendEntry(args)
	case "search":
		return t.search(args)
	default:
		return "", fmt.Errorf("unknown action %q, expected update/append/search", action)
	}
}

func (t *MemoryTool) update(args map[string]any) (string, error) {
	content, _ := args["content"].(string)
	if content == "" {
		return "", fmt.Errorf("content is required for update action")
	}
	if err := t.store.WriteFacts(content); err != nil {
		return "", fmt.Errorf("write facts: %w", err)
	}
	return "Memory updated.", nil
}

func (t *MemoryTool) appendEntry(args map[string]any) (string, error) {
	text, _ := args["text"].(string)
	if text == "" {
		return "", fmt.Errorf("text is required for append action")
	}

	var tags []string
	if raw, ok := args["tags"]; ok {
		switch v := raw.(type) {
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok {
					tags = append(tags, s)
				}
			}
		case []string:
			tags = v
		}
	}

	entry := JournalEntry{
		Timestamp: time.Now().UTC(),
		Tags:      tags,
		Text:      text,
	}
	if err := t.store.Append(entry); err != nil {
		return "", fmt.Errorf("append journal: %w", err)
	}
	return fmt.Sprintf("Journal entry added at %s.", entry.Timestamp.Format(time.RFC3339)), nil
}

func (t *MemoryTool) search(args map[string]any) (string, error) {
	query, _ := args["query"].(string)
	tag, _ := args["tag"].(string)
	limit := 20
	if v, ok := args["limit"].(float64); ok {
		limit = int(v)
	}

	entries, err := t.store.Search(query, tag, limit)
	if err != nil {
		return "", fmt.Errorf("search journal: %w", err)
	}
	if len(entries) == 0 {
		return "No matching journal entries found.", nil
	}

	out, _ := json.MarshalIndent(entries, "", "  ")
	return string(out), nil
}
