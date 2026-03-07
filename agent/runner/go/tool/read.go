package tool

import (
	"context"
	"fmt"
	"os"

	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

// ReadTool reads file contents.
type ReadTool struct{}

func (t *ReadTool) Definition() aitypes.ToolDefinition {
	return aitypes.ToolDefinition{
		Name:        "read",
		Description: "Read the contents of a file. Use this instead of cat or sed to examine files.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file_path": map[string]any{
					"type":        "string",
					"description": "Absolute or relative path to the file to read.",
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

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	tr := TruncateHead(string(data))
	return tr.Content, nil
}
