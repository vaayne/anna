package tool

import (
	"context"
	"fmt"
	"os"
	"strings"

	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

// ReadTool reads file contents.
type ReadTool struct{}

func (t *ReadTool) Definition() aitypes.ToolDefinition {
	return aitypes.ToolDefinition{
		Name:        "read",
		Description: "Read the contents of a file. Output is truncated to 2000 lines or 50KB. Use offset and limit to paginate through large files.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file_path": map[string]any{
					"type":        "string",
					"description": "Absolute or relative path to the file to read.",
				},
				"offset": map[string]any{
					"type":        "integer",
					"description": "Line number to start reading from (1-based). Defaults to 1.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum number of lines to read. Defaults to all lines.",
				},
			},
			"required": []string{"file_path"},
		},
	}
}

func (t *ReadTool) Execute(_ context.Context, args map[string]any) (string, error) {
	path, ok := args["file_path"].(string)
	if !ok || path == "" {
		return "", fmt.Errorf("read: file_path is required")
	}

	offset := intArg(args, "offset", 1)
	limit := intArg(args, "limit", 0)
	if offset < 1 {
		offset = 1
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	allLines := strings.SplitAfter(string(data), "\n")
	// SplitAfter produces a trailing empty element if file ends with \n.
	if len(allLines) > 0 && allLines[len(allLines)-1] == "" {
		allLines = allLines[:len(allLines)-1]
	}
	totalLines := len(allLines)

	// Apply offset (1-based).
	start := offset - 1
	if start > totalLines {
		start = totalLines
	}
	selected := allLines[start:]

	// Apply limit.
	if limit > 0 && limit < len(selected) {
		selected = selected[:limit]
	}

	content := strings.Join(selected, "")

	tr := TruncateHead(content)

	// Add pagination hint when there are more lines beyond what was returned.
	lastLineShown := offset + tr.OutputLines - 1
	if lastLineShown < totalLines {
		hint := fmt.Sprintf("\n[Use offset=%d to continue reading]", lastLineShown+1)
		tr.Content += hint
	}

	return tr.Content, nil
}

func intArg(args map[string]any, key string, defaultVal int) int {
	v, ok := args[key]
	if !ok {
		return defaultVal
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return defaultVal
	}
}
