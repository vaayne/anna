package telegram

import (
	"strings"
	"testing"

	tgmd "github.com/Mad-Pixels/goldmark-tgmd"
)

func TestSplitMessageShort(t *testing.T) {
	chunks := splitMessage("hello")
	if len(chunks) != 1 || chunks[0] != "hello" {
		t.Errorf("chunks = %v, want [hello]", chunks)
	}
}

func TestSplitMessageExactLimit(t *testing.T) {
	msg := strings.Repeat("a", telegramMaxMessageLen)
	chunks := splitMessage(msg)
	if len(chunks) != 1 {
		t.Errorf("len(chunks) = %d, want 1", len(chunks))
	}
}

func TestSplitMessageLong(t *testing.T) {
	msg := strings.Repeat("a", telegramMaxMessageLen+100)
	chunks := splitMessage(msg)
	if len(chunks) != 2 {
		t.Errorf("len(chunks) = %d, want 2", len(chunks))
	}
	if len(chunks[0]) != telegramMaxMessageLen {
		t.Errorf("chunk[0] len = %d, want %d", len(chunks[0]), telegramMaxMessageLen)
	}
	if len(chunks[1]) != 100 {
		t.Errorf("chunk[1] len = %d, want 100", len(chunks[1]))
	}
}

func TestSplitMessageAtNewline(t *testing.T) {
	part1 := strings.Repeat("a", 3000)
	part2 := strings.Repeat("b", 2000)
	msg := part1 + "\n" + part2

	chunks := splitMessage(msg)
	if len(chunks) != 2 {
		t.Fatalf("len(chunks) = %d, want 2", len(chunks))
	}
	if chunks[0] != part1+"\n" {
		t.Errorf("chunk[0] = %q..., want split at newline", chunks[0][:20])
	}
	if chunks[1] != part2 {
		t.Errorf("chunk[1] len = %d, want %d", len(chunks[1]), len(part2))
	}
}

func TestSplitMessageEmpty(t *testing.T) {
	chunks := splitMessage("")
	if len(chunks) != 1 || chunks[0] != "" {
		t.Errorf("chunks = %v, want [\"\"]", chunks)
	}
}

func TestSplitMessageMultipleChunks(t *testing.T) {
	msg := strings.Repeat("x", telegramMaxMessageLen*3+500)
	chunks := splitMessage(msg)
	if len(chunks) != 4 {
		t.Errorf("len(chunks) = %d, want 4", len(chunks))
	}
	var rebuilt strings.Builder
	for _, c := range chunks {
		rebuilt.WriteString(c)
	}
	if rebuilt.String() != msg {
		t.Error("chunks do not reconstruct to original message")
	}
}

func TestRenderMarkdown(t *testing.T) {
	md := tgmd.TGMD()

	tests := []struct {
		name  string
		input string
	}{
		{"bold", "**bold text**"},
		{"code block", "```go\nfmt.Println()\n```"},
		{"plain text", "just plain text"},
		{"empty", " "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := renderMarkdown(md, tt.input)
			if result == "" {
				t.Error("renderMarkdown returned empty string")
			}
		})
	}
}

func TestRenderMarkdownFallback(t *testing.T) {
	md := tgmd.TGMD()
	// Plain text should still return something non-empty.
	result := renderMarkdown(md, "hello world")
	if result == "" {
		t.Error("expected non-empty result for plain text")
	}
}
