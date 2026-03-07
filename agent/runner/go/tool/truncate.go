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

// TruncationResult holds the truncated output and metadata.
type TruncationResult struct {
	Content      string
	Truncated    bool
	TotalLines   int
	TotalBytes   int
	OutputLines  int
	OutputBytes  int
	FullFilePath string
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

// TruncateHead keeps the first N lines / bytes (whichever limit is hit first).
// Suitable for file reads and search results where the beginning matters most.
func TruncateHead(output string) TruncationResult {
	lineLimit := maxLines()
	byteLimit := maxBytes()
	totalBytes := len(output)
	lines := strings.SplitAfter(output, "\n")
	// SplitAfter may produce a trailing empty element if output ends with \n.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	totalLines := len(lines)

	if totalLines <= lineLimit && totalBytes <= byteLimit {
		return TruncationResult{Content: output, TotalLines: totalLines, TotalBytes: totalBytes, OutputLines: totalLines, OutputBytes: totalBytes}
	}

	// Keep lines from the head until we hit a limit.
	var kept []string
	keptBytes := 0
	for _, line := range lines {
		if len(kept) >= lineLimit || keptBytes+len(line) > byteLimit {
			break
		}
		kept = append(kept, line)
		keptBytes += len(line)
	}

	truncated := strings.Join(kept, "")
	fullPath := saveTempFile(output)
	if fullPath == "" {
		return TruncationResult{Content: output, TotalLines: totalLines, TotalBytes: totalBytes, OutputLines: totalLines, OutputBytes: totalBytes}
	}

	content := formatTruncatedHead(truncated, totalLines, totalBytes, len(kept), fullPath)
	return TruncationResult{
		Content:      content,
		Truncated:    true,
		TotalLines:   totalLines,
		TotalBytes:   totalBytes,
		OutputLines:  len(kept),
		OutputBytes:  keptBytes,
		FullFilePath: fullPath,
	}
}

// TruncateTail keeps the last N lines / bytes (whichever limit is hit first).
// Suitable for command output and logs where the end matters most (errors, final results).
func TruncateTail(output string) TruncationResult {
	lineLimit := maxLines()
	byteLimit := maxBytes()
	totalBytes := len(output)
	lines := strings.SplitAfter(output, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	totalLines := len(lines)

	if totalLines <= lineLimit && totalBytes <= byteLimit {
		return TruncationResult{Content: output, TotalLines: totalLines, TotalBytes: totalBytes, OutputLines: totalLines, OutputBytes: totalBytes}
	}

	// Keep lines from the tail until we hit a limit.
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

	truncated := strings.Join(kept, "")
	fullPath := saveTempFile(output)
	if fullPath == "" {
		return TruncationResult{Content: output, TotalLines: totalLines, TotalBytes: totalBytes, OutputLines: totalLines, OutputBytes: totalBytes}
	}

	content := formatTruncatedTail(truncated, totalLines, totalBytes, len(kept), fullPath)
	return TruncationResult{
		Content:      content,
		Truncated:    true,
		TotalLines:   totalLines,
		TotalBytes:   totalBytes,
		OutputLines:  len(kept),
		OutputBytes:  keptBytes,
		FullFilePath: fullPath,
	}
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

func formatTruncatedHead(truncated string, totalLines, totalBytes, shownLines int, fullPath string) string {
	header := fmt.Sprintf("[Output truncated — showing first %d of %d lines (%d bytes total)]", shownLines, totalLines, totalBytes)
	footer := truncationFooter(fullPath)
	return header + "\n\n" + truncated + "\n...\n\n" + footer
}

func formatTruncatedTail(truncated string, totalLines, totalBytes, shownLines int, fullPath string) string {
	header := fmt.Sprintf("[Output truncated — showing last %d of %d lines (%d bytes total)]", shownLines, totalLines, totalBytes)
	footer := truncationFooter(fullPath)
	return header + "\n\n...\n" + truncated + "\n" + footer
}

func truncationFooter(fullPath string) string {
	if fullPath == "" {
		return "[Could not save full output to temp file]"
	}
	return fmt.Sprintf("[Full output saved to %s — use the read tool to access]", fullPath)
}
