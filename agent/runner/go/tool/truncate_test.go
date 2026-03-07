package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- TruncateHead tests ---

func TestTruncateHeadShortOutput(t *testing.T) {
	input := "line1\nline2\nline3\n"
	r := TruncateHead(input)
	if r.Truncated {
		t.Error("short output should not be truncated")
	}
	if r.Content != input {
		t.Errorf("content = %q, want %q", r.Content, input)
	}
}

func TestTruncateHeadEmpty(t *testing.T) {
	r := TruncateHead("")
	if r.Truncated {
		t.Error("empty output should not be truncated")
	}
	if r.Content != "" {
		t.Errorf("content = %q, want empty", r.Content)
	}
}

func TestTruncateHeadByLines(t *testing.T) {
	t.Setenv("ANNA_TOOL_MAX_LINES", "3")
	t.Setenv("ANNA_TOOL_MAX_BYTES", "999999")

	lines := make([]string, 10)
	for i := range lines {
		lines[i] = "line\n"
	}
	input := strings.Join(lines, "")

	r := TruncateHead(input)
	if !r.Truncated {
		t.Fatal("should be truncated")
	}
	if r.TotalLines != 10 {
		t.Errorf("TotalLines = %d, want 10", r.TotalLines)
	}
	if r.OutputLines != 3 {
		t.Errorf("OutputLines = %d, want 3", r.OutputLines)
	}
	if !strings.Contains(r.Content, "showing first 3 of 10 lines") {
		t.Errorf("header missing, got: %s", r.Content)
	}
	// Verify temp file.
	verifyTempFile(t, r.FullFilePath, input)
}

func TestTruncateHeadByBytes(t *testing.T) {
	t.Setenv("ANNA_TOOL_MAX_LINES", "999999")
	t.Setenv("ANNA_TOOL_MAX_BYTES", "20")

	// Each line is 6 bytes ("abcde\n"). 20 bytes fits 3 lines (18 bytes), 4th would exceed.
	lines := make([]string, 10)
	for i := range lines {
		lines[i] = "abcde\n"
	}
	input := strings.Join(lines, "")

	r := TruncateHead(input)
	if !r.Truncated {
		t.Fatal("should be truncated by bytes")
	}
	if r.OutputLines != 3 {
		t.Errorf("OutputLines = %d, want 3", r.OutputLines)
	}
	verifyTempFile(t, r.FullFilePath, input)
}

func TestTruncateHeadExactThreshold(t *testing.T) {
	t.Setenv("ANNA_TOOL_MAX_LINES", "5")
	t.Setenv("ANNA_TOOL_MAX_BYTES", "999999")

	lines := make([]string, 5)
	for i := range lines {
		lines[i] = "line\n"
	}
	input := strings.Join(lines, "")

	r := TruncateHead(input)
	if r.Truncated {
		t.Error("output at exactly the line threshold should not be truncated")
	}
}

// --- TruncateTail tests ---

func TestTruncateTailShortOutput(t *testing.T) {
	input := "line1\nline2\n"
	r := TruncateTail(input)
	if r.Truncated {
		t.Error("short output should not be truncated")
	}
	if r.Content != input {
		t.Errorf("content = %q, want %q", r.Content, input)
	}
}

func TestTruncateTailByLines(t *testing.T) {
	t.Setenv("ANNA_TOOL_MAX_LINES", "3")
	t.Setenv("ANNA_TOOL_MAX_BYTES", "999999")

	lines := make([]string, 10)
	for i := range lines {
		lines[i] = "line\n"
	}
	input := strings.Join(lines, "")

	r := TruncateTail(input)
	if !r.Truncated {
		t.Fatal("should be truncated")
	}
	if r.OutputLines != 3 {
		t.Errorf("OutputLines = %d, want 3", r.OutputLines)
	}
	if !strings.Contains(r.Content, "showing last 3 of 10 lines") {
		t.Errorf("header missing, got: %s", r.Content)
	}
	// The kept content should be the last 3 lines.
	if !strings.Contains(r.Content, "line\nline\nline\n") {
		t.Error("should contain the last 3 lines")
	}
	verifyTempFile(t, r.FullFilePath, input)
}

func TestTruncateTailByBytes(t *testing.T) {
	t.Setenv("ANNA_TOOL_MAX_LINES", "999999")
	t.Setenv("ANNA_TOOL_MAX_BYTES", "20")

	lines := make([]string, 10)
	for i := range lines {
		lines[i] = "abcde\n"
	}
	input := strings.Join(lines, "")

	r := TruncateTail(input)
	if !r.Truncated {
		t.Fatal("should be truncated by bytes")
	}
	if r.OutputLines != 3 {
		t.Errorf("OutputLines = %d, want 3", r.OutputLines)
	}
	verifyTempFile(t, r.FullFilePath, input)
}

// --- Env var override tests ---

func TestMaxLinesDefault(t *testing.T) {
	n := maxLines()
	if n != defaultMaxLines {
		t.Errorf("default maxLines = %d, want %d", n, defaultMaxLines)
	}
}

func TestMaxLinesEnvOverride(t *testing.T) {
	t.Setenv("ANNA_TOOL_MAX_LINES", "500")
	if n := maxLines(); n != 500 {
		t.Errorf("maxLines = %d, want 500", n)
	}
}

func TestMaxLinesEnvInvalid(t *testing.T) {
	t.Setenv("ANNA_TOOL_MAX_LINES", "bad")
	if n := maxLines(); n != defaultMaxLines {
		t.Errorf("maxLines = %d, want %d", n, defaultMaxLines)
	}
}

func TestMaxBytesDefault(t *testing.T) {
	n := maxBytes()
	if n != defaultMaxBytes {
		t.Errorf("default maxBytes = %d, want %d", n, defaultMaxBytes)
	}
}

func TestMaxBytesEnvOverride(t *testing.T) {
	t.Setenv("ANNA_TOOL_MAX_BYTES", "1024")
	if n := maxBytes(); n != 1024 {
		t.Errorf("maxBytes = %d, want 1024", n)
	}
}

// --- Integration: tools apply truncation ---

func TestBashToolTruncatesOutput(t *testing.T) {
	t.Setenv("ANNA_TOOL_MAX_LINES", "3")
	t.Setenv("ANNA_TOOL_MAX_BYTES", "999999")

	tool := &BashTool{}
	// Generate 10 lines of output.
	result, err := tool.Execute(context.Background(), map[string]any{
		"command": "for i in $(seq 1 10); do echo line$i; done",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Output truncated") {
		t.Error("bash output should be truncated")
	}
	// Tail truncation: should show the last lines.
	if !strings.Contains(result, "showing last") {
		t.Errorf("should use tail truncation, got: %s", result)
	}
}

func TestReadToolTruncatesOutput(t *testing.T) {
	t.Setenv("ANNA_TOOL_MAX_LINES", "3")
	t.Setenv("ANNA_TOOL_MAX_BYTES", "999999")

	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	lines := make([]string, 10)
	for i := range lines {
		lines[i] = "line\n"
	}
	os.WriteFile(path, []byte(strings.Join(lines, "")), 0o644)

	tool := &ReadTool{}
	result, err := tool.Execute(context.Background(), map[string]any{"file_path": path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Output truncated") {
		t.Error("read output should be truncated")
	}
	// Head truncation: should show the first lines.
	if !strings.Contains(result, "showing first") {
		t.Errorf("should use head truncation, got: %s", result)
	}
}

func TestBashToolNoTruncateOnError(t *testing.T) {
	reg := NewRegistry("")
	_, err := reg.Execute(context.Background(), "bash", map[string]any{"command": "echo 'fail'; exit 1"})
	if err == nil {
		t.Fatal("expected error from failing command")
	}
}

// --- helpers ---

func verifyTempFile(t *testing.T, path, expectedContent string) {
	t.Helper()
	if path == "" {
		t.Fatal("expected temp file path, got empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read temp file %s: %v", path, err)
	}
	if string(data) != expectedContent {
		t.Error("temp file should contain the full original output")
	}
	t.Cleanup(func() { os.Remove(path) })
}
