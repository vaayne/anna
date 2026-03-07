package tool

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	defaultMaxLines = 2000
	defaultMaxBytes = 50 * 1024 // 50KB
)

// truncationResult holds the truncated output and metadata.
type truncationResult struct {
	Content     string
	OutputLines int
}

func maxLines() int {
	if v := os.Getenv("ANNA_TOOL_MAX_LINES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxLines
}

func maxBytes() int {
	if v := os.Getenv("ANNA_TOOL_MAX_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxBytes
}

// splitLines splits text into lines preserving newline suffixes.
// Trims the trailing empty element that strings.SplitAfter produces.
func splitLines(text string) []string {
	lines := strings.SplitAfter(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// TruncateHead keeps the first N lines / bytes (whichever limit is hit first).
// Suitable for file reads and search results where the beginning matters most.
func TruncateHead(output string) truncationResult {
	return truncate(output, "first", keepHead)
}

// TruncateTail keeps the last N lines / bytes (whichever limit is hit first).
// Suitable for command output and logs where the end matters most.
func TruncateTail(output string) truncationResult {
	return truncate(output, "last", keepTail)
}

// keepFunc selects which lines to keep given the full set and limits.
type keepFunc func(lines []string, lineLimit, byteLimit int) []string

func truncate(output, direction string, keep keepFunc) truncationResult {
	lineLimit := maxLines()
	byteLimit := maxBytes()
	totalBytes := len(output)

	lines := splitLines(output)
	totalLines := len(lines)

	if totalLines <= lineLimit && totalBytes <= byteLimit {
		return truncationResult{Content: output, OutputLines: totalLines}
	}

	kept := keep(lines, lineLimit, byteLimit)
	truncated := strings.Join(kept, "")

	fullPath := saveTempFile(output)
	if fullPath == "" {
		return truncationResult{Content: output, OutputLines: totalLines}
	}

	content := formatTruncated(truncated, direction, totalLines, totalBytes, len(kept), fullPath)
	return truncationResult{Content: content, OutputLines: len(kept)}
}

func keepHead(lines []string, lineLimit, byteLimit int) []string {
	var kept []string
	keptBytes := 0
	for _, line := range lines {
		if len(kept) >= lineLimit || keptBytes+len(line) > byteLimit {
			break
		}
		kept = append(kept, line)
		keptBytes += len(line)
	}
	return kept
}

func keepTail(lines []string, lineLimit, byteLimit int) []string {
	var kept []string
	keptBytes := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if len(kept) >= lineLimit || keptBytes+len(lines[i]) > byteLimit {
			break
		}
		kept = append(kept, lines[i])
		keptBytes += len(lines[i])
	}
	// Reverse to restore original order.
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return kept
}

func saveTempFile(output string) string {
	tmpFile, err := os.CreateTemp("", "anna-tool-*.txt")
	if err != nil {
		return ""
	}
	defer tmpFile.Close()
	if _, err := tmpFile.WriteString(output); err != nil {
		os.Remove(tmpFile.Name())
		return ""
	}
	return tmpFile.Name()
}

func formatTruncated(truncated, direction string, totalLines, totalBytes, shownLines int, fullPath string) string {
	header := fmt.Sprintf("[Output truncated — showing %s %d of %d lines (%d bytes total)]", direction, shownLines, totalLines, totalBytes)
	footer := fmt.Sprintf("[Full output saved to %s — use the read tool to access]", fullPath)
	if direction == "last" {
		return header + "\n\n...\n" + truncated + "\n" + footer
	}
	return header + "\n\n" + truncated + "\n...\n\n" + footer
}
