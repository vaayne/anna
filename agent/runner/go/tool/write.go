package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

// WriteTool creates new files or completely overwrites existing ones.
type WriteTool struct{}

func (t *WriteTool) Definition() aitypes.ToolDefinition {
	return aitypes.ToolDefinition{
		Name:        "write",
		Description: "Create a new file or completely overwrite an existing file with the provided content.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file_path": map[string]any{
					"type":        "string",
					"description": "Path to the file to create or overwrite.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "The full content to write to the file.",
				},
			},
			"required": []string{"file_path", "content"},
		},
	}
}

func (t *WriteTool) Execute(_ context.Context, args map[string]any) (string, error) {
	path, _ := args["file_path"].(string)
	content, _ := args["content"].(string)

	if path == "" {
		return "", fmt.Errorf("write: file_path is required")
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("write: mkdir %s: %w", dir, err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}

	return fmt.Sprintf("Wrote %s (%d bytes)", path, len(content)), nil
}
