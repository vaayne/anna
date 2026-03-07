package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- TruncateHead tests ---

func TestTruncateHeadShortOutput(t *testing.T) {
	input := "line1\nline2\nline3\n"
	r := TruncateHead(input)
	if r.Content != input {
		t.Errorf("content = %q, want %q", r.Content, input)
	}
}

func TestTruncateHeadEmpty(t *testing.T) {
	r := TruncateHead("")
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
	if r.OutputLines != 3 {
		t.Errorf("OutputLines = %d, want 3", r.OutputLines)
	}
	if !strings.Contains(r.Content, "showing first 3 of 10 lines") {
		t.Errorf("header missing, got: %s", r.Content)
	}
	verifyTempFileInContent(t, r.Content, input)
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
	if r.OutputLines != 3 {
		t.Errorf("OutputLines = %d, want 3", r.OutputLines)
	}
	verifyTempFileInContent(t, r.Content, input)
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
	if strings.Contains(r.Content, "Output truncated") {
		t.Error("output at exactly the line threshold should not be truncated")
	}
}

// --- TruncateTail tests ---

func TestTruncateTailShortOutput(t *testing.T) {
	input := "line1\nline2\n"
	r := TruncateTail(input)
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
	if r.OutputLines != 3 {
		t.Errorf("OutputLines = %d, want 3", r.OutputLines)
	}
	if !strings.Contains(r.Content, "showing last 3 of 10 lines") {
		t.Errorf("header missing, got: %s", r.Content)
	}
	if !strings.Contains(r.Content, "line\nline\nline\n") {
		t.Error("should contain the last 3 lines")
	}
	verifyTempFileInContent(t, r.Content, input)
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
	if r.OutputLines != 3 {
		t.Errorf("OutputLines = %d, want 3", r.OutputLines)
	}
	verifyTempFileInContent(t, r.Content, input)
}

// --- Env var override tests ---

func TestMaxLinesDefault(t *testing.T) {
	if n := maxLines(); n != defaultMaxLines {
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
	if n := maxBytes(); n != defaultMaxBytes {
		t.Errorf("default maxBytes = %d, want %d", n, defaultMaxBytes)
	}
}

func TestMaxBytesEnvOverride(t *testing.T) {
	t.Setenv("ANNA_TOOL_MAX_BYTES", "1024")
	if n := maxBytes(); n != 1024 {
		t.Errorf("maxBytes = %d, want 1024", n)
	}
}

// --- splitLines tests ---

func TestSplitLines(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"", 0},
		{"a\n", 1},
		{"a\nb\n", 2},
		{"a\nb", 2}, // no trailing newline
		{"a\nb\nc\n", 3},
	}
	for _, tt := range tests {
		lines := splitLines(tt.input)
		if len(lines) != tt.want {
			t.Errorf("splitLines(%q) = %d lines, want %d", tt.input, len(lines), tt.want)
		}
	}
}

// --- Integration: tools apply truncation ---

func TestBashToolTruncatesOutput(t *testing.T) {
	t.Setenv("ANNA_TOOL_MAX_LINES", "3")
	t.Setenv("ANNA_TOOL_MAX_BYTES", "999999")

	tool := &BashTool{}
	result, err := tool.Execute(context.Background(), map[string]any{
		"command": "for i in $(seq 1 10); do echo line$i; done",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Output truncated") {
		t.Error("bash output should be truncated")
	}
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
	if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &ReadTool{}
	result, err := tool.Execute(context.Background(), map[string]any{"file_path": path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Output truncated") {
		t.Error("read output should be truncated")
	}
	if !strings.Contains(result, "showing first") {
		t.Errorf("should use head truncation, got: %s", result)
	}
}

func TestReadToolWithOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lines.txt")
	if err := os.WriteFile(path, []byte("line1\nline2\nline3\nline4\nline5\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &ReadTool{}
	result, err := tool.Execute(context.Background(), map[string]any{
		"file_path": path,
		"offset":    float64(3),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "line3") {
		t.Errorf("should contain line3, got: %s", result)
	}
	if strings.Contains(result, "line1") {
		t.Errorf("should not contain line1, got: %s", result)
	}
}

func TestReadToolWithLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lines.txt")
	if err := os.WriteFile(path, []byte("line1\nline2\nline3\nline4\nline5\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &ReadTool{}
	result, err := tool.Execute(context.Background(), map[string]any{
		"file_path": path,
		"limit":     float64(2),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "line1") || !strings.Contains(result, "line2") {
		t.Errorf("should contain first 2 lines, got: %s", result)
	}
	if strings.Contains(result, "line3\n") {
		t.Errorf("should not contain line3 content, got: %s", result)
	}
	if !strings.Contains(result, "Use offset=3 to continue") {
		t.Errorf("should contain pagination hint, got: %s", result)
	}
}

func TestReadToolWithOffsetAndLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lines.txt")
	if err := os.WriteFile(path, []byte("line1\nline2\nline3\nline4\nline5\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &ReadTool{}
	result, err := tool.Execute(context.Background(), map[string]any{
		"file_path": path,
		"offset":    float64(2),
		"limit":     float64(2),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "line2") || !strings.Contains(result, "line3") {
		t.Errorf("should contain lines 2-3, got: %s", result)
	}
	if !strings.Contains(result, "Use offset=4 to continue") {
		t.Errorf("should contain pagination hint, got: %s", result)
	}
}

func TestReadToolPaginationHintNotShownAtEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lines.txt")
	if err := os.WriteFile(path, []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &ReadTool{}
	result, err := tool.Execute(context.Background(), map[string]any{"file_path": path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(result, "Use offset=") {
		t.Errorf("should not show pagination hint when all lines are shown, got: %s", result)
	}
}

func TestReadToolTruncatedShowsPaginationHint(t *testing.T) {
	t.Setenv("ANNA_TOOL_MAX_LINES", "3")
	t.Setenv("ANNA_TOOL_MAX_BYTES", "999999")

	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	var content string
	for i := 1; i <= 10; i++ {
		content += fmt.Sprintf("line%d\n", i)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &ReadTool{}
	result, err := tool.Execute(context.Background(), map[string]any{"file_path": path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Use offset=4 to continue") {
		t.Errorf("truncated read should show pagination hint, got: %s", result)
	}
}

func TestReadToolLongLine(t *testing.T) {
	// File with a single line longer than the scanner buffer (1MB).
	// Should fall back to os.ReadFile and not error.
	dir := t.TempDir()
	path := filepath.Join(dir, "longline.txt")
	longLine := strings.Repeat("x", 2*1024*1024) // 2MB single line
	if err := os.WriteFile(path, []byte(longLine), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &ReadTool{}
	result, err := tool.Execute(context.Background(), map[string]any{"file_path": path})
	if err != nil {
		t.Fatalf("should not error on long lines, got: %v", err)
	}
	if result == "" {
		t.Error("should return content for long line file")
	}
}

func TestReadToolPaginationAdvancesOnZeroOutputLines(t *testing.T) {
	// When a single line exceeds the byte limit, OutputLines == 0.
	// Pagination should still advance by 1 to avoid infinite loop.
	t.Setenv("ANNA_TOOL_MAX_LINES", "999999")
	t.Setenv("ANNA_TOOL_MAX_BYTES", "10") // Very small byte limit

	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	// Single line of 50 bytes + a second line.
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 50)+"\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &ReadTool{}
	result, err := tool.Execute(context.Background(), map[string]any{"file_path": path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should suggest offset=2, not offset=1 (which would loop).
	if strings.Contains(result, "Use offset=1") {
		t.Errorf("pagination should advance past current offset, got: %s", result)
	}
	if !strings.Contains(result, "Use offset=2") {
		t.Errorf("should suggest offset=2, got: %s", result)
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

// verifyTempFileInContent extracts the temp file path from truncation output
// and verifies it contains the expected content.
func verifyTempFileInContent(t *testing.T, content, expectedContent string) {
	t.Helper()
	// Format: [Full output saved to /tmp/anna-tool-xxx.txt — use the read tool to access]
	idx := strings.Index(content, "/tmp/anna-tool-")
	if idx < 0 {
		// Also check for other temp dir patterns.
		idx = strings.Index(content, "/var/")
		if idx < 0 {
			t.Fatal("could not find temp file path in output")
		}
	}
	end := strings.Index(content[idx:], " — use")
	if end < 0 {
		t.Fatal("could not find end of temp file path in output")
	}
	tmpPath := content[idx : idx+end]

	data, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatalf("failed to read temp file %s: %v", tmpPath, err)
	}
	if string(data) != expectedContent {
		t.Error("temp file should contain the full original output")
	}
	t.Cleanup(func() { _ = os.Remove(tmpPath) })
}
