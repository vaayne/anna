package tool

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"

	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

// BashTool executes bash commands.
type BashTool struct {
	workDir string
}

func (t *BashTool) Definition() aitypes.ToolDefinition {
	return aitypes.ToolDefinition{
		Name:        "bash",
		Description: "Execute a bash command. Use for file operations like ls, rg, find, git, and other shell commands.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The bash command to execute.",
				},
			},
			"required": []string{"command"},
		},
	}
}

func (t *BashTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	command, ok := args["command"].(string)
	if !ok || command == "" {
		return "", fmt.Errorf("bash: command is required")
	}

	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	if t.workDir != "" {
		cmd.Dir = t.workDir
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	result := stdout.String()
	if stderr.Len() > 0 {
		if result != "" {
			result += "\n"
		}
		result += stderr.String()
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return result, fmt.Errorf("bash: exit code %d", exitErr.ExitCode())
		}
		return result, fmt.Errorf("bash: %w", err)
	}
	tr := TruncateTail(result)
	return tr.Content, nil
}
