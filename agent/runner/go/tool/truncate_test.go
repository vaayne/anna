package tool

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestTruncateShortOutput(t *testing.T) {
	input := "hello world this is short"
	result := truncateIfNeeded(input)
	if result != input {
		t.Errorf("short output should pass through unchanged, got %q", result)
	}
}

func TestTruncateEmptyOutput(t *testing.T) {
	result := truncateIfNeeded("")
	if result != "" {
		t.Errorf("empty output should pass through unchanged, got %q", result)
	}
}

func TestTruncateLongOutput(t *testing.T) {
	// Generate output with 1500 words.
	words := make([]string, 1500)
	for i := range words {
		words[i] = "word"
	}
	input := strings.Join(words, " ")

	result := truncateIfNeeded(input)

	if !strings.HasPrefix(result, "[Output truncated") {
		t.Error("truncated output should start with truncation header")
	}
	if !strings.Contains(result, "showing first ~1000 words of 1500 total") {
		t.Error("truncated output should contain word counts")
	}
	if !strings.Contains(result, "anna-tool-") {
		t.Error("truncated output should contain temp file path")
	}
	if !strings.Contains(result, "use the read tool to access") {
		t.Error("truncated output should contain read tool hint")
	}

	// Verify the temp file contains the full output.
	// Extract the file path from the last line.
	lines := strings.Split(result, "\n")
	lastLine := lines[len(lines)-1]
	// Format: [Full output saved to /tmp/anna-tool-xxx.txt — use the read tool to access]
	pathStart := strings.Index(lastLine, "/")
	pathEnd := strings.Index(lastLine, " — use")
	if pathStart < 0 || pathEnd < 0 {
		t.Fatal("could not extract temp file path from output")
	}
	tmpPath := lastLine[pathStart:pathEnd]

	data, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatalf("failed to read temp file: %v", err)
	}
	if string(data) != input {
		t.Error("temp file should contain the full original output")
	}

	// Cleanup.
	os.Remove(tmpPath)
}

func TestTruncateExactThreshold(t *testing.T) {
	words := make([]string, 1000)
	for i := range words {
		words[i] = "word"
	}
	input := strings.Join(words, " ")

	result := truncateIfNeeded(input)
	if result != input {
		t.Error("output at exactly the threshold should not be truncated")
	}
}

func TestTruncateEnvOverride(t *testing.T) {
	t.Setenv("ANNA_TOOL_MAX_WORDS", "5")

	input := "one two three four five six seven"
	result := truncateIfNeeded(input)

	if !strings.HasPrefix(result, "[Output truncated") {
		t.Error("should truncate when over env var threshold")
	}
	if !strings.Contains(result, "showing first ~5 words of 7 total") {
		t.Errorf("should use env var threshold, got: %s", result)
	}

	// Extract and cleanup temp file.
	lines := strings.Split(result, "\n")
	lastLine := lines[len(lines)-1]
	pathStart := strings.Index(lastLine, "/")
	pathEnd := strings.Index(lastLine, " — use")
	if pathStart >= 0 && pathEnd >= 0 {
		os.Remove(lastLine[pathStart:pathEnd])
	}
}

func TestTruncateEnvInvalid(t *testing.T) {
	t.Setenv("ANNA_TOOL_MAX_WORDS", "notanumber")

	n := maxOutputWords()
	if n != defaultMaxOutputWords {
		t.Errorf("invalid env should fall back to default, got %d", n)
	}
}

func TestTruncateRegistrySkipsErrors(t *testing.T) {
	// Verify that Execute does not truncate error results.
	reg := NewRegistry("")
	// Execute a failing bash command that would produce output.
	_, err := reg.Execute(context.Background(), "bash", map[string]any{"command": "echo 'fail'; exit 1"})
	if err == nil {
		t.Fatal("expected error from failing command")
	}
	// Error results should not go through truncation — they're returned as errors.
}
