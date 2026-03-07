package tool

import (
	"context"
	"fmt"
	"os"
	"strings"

	aitypes "github.com/vaayne/anna/ai/types"
)

// EditTool makes surgical edits to files by exact string replacement.
type EditTool struct{}

func (t *EditTool) Definition() aitypes.ToolDefinition {
	return aitypes.ToolDefinition{
		Name:        "edit",
		Description: "Make a surgical edit to a file. The old_string must match exactly (including whitespace and indentation). Use this for targeted changes to existing files.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file_path": map[string]any{
					"type":        "string",
					"description": "Path to the file to edit.",
				},
				"old_string": map[string]any{
					"type":        "string",
					"description": "The exact text to find and replace. Must match the file content exactly.",
				},
				"new_string": map[string]any{
					"type":        "string",
					"description": "The replacement text.",
				},
			},
			"required": []string{"file_path", "old_string", "new_string"},
		},
	}
}

func (t *EditTool) Execute(_ context.Context, args map[string]any) (string, error) {
	path, _ := args["file_path"].(string)
	oldStr, _ := args["old_string"].(string)
	newStr, _ := args["new_string"].(string)

	if path == "" {
		return "", fmt.Errorf("edit: file_path is required")
	}
	if oldStr == "" {
		return "", fmt.Errorf("edit: old_string is required")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("edit: read %s: %w", path, err)
	}

	content := string(data)
	count := strings.Count(content, oldStr)
	if count == 0 {
		return "", fmt.Errorf("edit: old_string not found in %s", path)
	}
	if count > 1 {
		return "", fmt.Errorf("edit: old_string matches %d times in %s (must be unique)", count, path)
	}

	updated := strings.Replace(content, oldStr, newStr, 1)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return "", fmt.Errorf("edit: write %s: %w", path, err)
	}

	return fmt.Sprintf("Edited %s", path), nil
}
