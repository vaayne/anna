package tool

import (
	"bufio"
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

	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()

	// Stream through the file: skip to offset, collect up to limit lines,
	// then count remaining lines without storing them.
	var lines []string
	totalLines := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		totalLines++
		if totalLines < offset {
			continue
		}
		if limit > 0 && len(lines) >= limit {
			continue
		}
		lines = append(lines, scanner.Text()+"\n")
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	// bufio.Scanner strips newlines; we added them back above.
	// If we collected the last line and file doesn't end with \n, trim it.
	if len(lines) > 0 && totalLines == offset+len(lines)-1 {
		if !endsWithNewline(f) {
			lines[len(lines)-1] = strings.TrimSuffix(lines[len(lines)-1], "\n")
		}
	}

	content := strings.Join(lines, "")
	tr := TruncateHead(content)

	lastLineShown := offset + tr.OutputLines - 1
	if lastLineShown < totalLines {
		hint := fmt.Sprintf("\n[Use offset=%d to continue reading]", lastLineShown+1)
		tr.Content += hint
	}

	return tr.Content, nil
}

// endsWithNewline checks whether the already-open file ends with a newline.
func endsWithNewline(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return false
	}
	buf := make([]byte, 1)
	if _, err := f.ReadAt(buf, fi.Size()-1); err != nil {
		return false
	}
	return buf[0] == '\n'
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
