package tool

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const defaultMaxOutputWords = 1000

func maxOutputWords() int {
	if v := os.Getenv("ANNA_TOOL_MAX_WORDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxOutputWords
}

func truncateIfNeeded(output string) string {
	limit := maxOutputWords()
	words := strings.Fields(output)
	if len(words) <= limit {
		return output
	}

	tmpFile, err := os.CreateTemp("", "anna-tool-*.txt")
	if err != nil {
		// If we can't write the temp file, return the original output.
		return output
	}
	defer tmpFile.Close()

	if _, err := tmpFile.WriteString(output); err != nil {
		os.Remove(tmpFile.Name())
		return output
	}

	truncated := strings.Join(words[:limit], " ")
	return fmt.Sprintf(
		"[Output truncated — showing first ~%d words of %d total]\n\n%s\n\n...\n\n[Full output saved to %s — use the read tool to access]",
		limit, len(words), truncated, tmpFile.Name(),
	)
}
