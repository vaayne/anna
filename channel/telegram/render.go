package telegram

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/yuin/goldmark/parser"
	tele "gopkg.in/telebot.v4"
)

// goldmarkMD is the interface satisfied by the goldmark Markdown converter.
type goldmarkMD interface {
	Convert(source []byte, w io.Writer, opts ...parser.ParseOption) error
}

// sendFinalResponse sends the completed response with markdown rendering,
// splitting into chunks if necessary.
func (b *Bot) sendFinalResponse(c tele.Context, response string) {
	if err := b.sendChunkedMarkdown(c.Chat(), response, false, nil); err != nil {
		logger().Error("sendFinalResponse failed", "chat_id", c.Chat().ID, "error", err)
	}
}

// sendChunkedMarkdown splits text into chunks, renders each as Telegram
// MarkdownV2, and falls back to plain text on error. If sendOpts is non-nil
// it is used for the markdown send attempt. Returns the first send error
// that could not be recovered via plain-text fallback.
func (b *Bot) sendChunkedMarkdown(chat tele.Recipient, text string, silent bool, sendOpts *tele.SendOptions) error {
	if sendOpts == nil {
		sendOpts = &tele.SendOptions{ParseMode: tele.ModeMarkdownV2}
	}
	chunks := splitMessage(text)
	for _, chunk := range chunks {
		rendered := renderMarkdown(b.md, chunk)
		if _, err := b.bot.Send(chat, rendered, sendOpts); err != nil {
			logger().Warn("markdown send failed, falling back to plain text", "error", err)
			plainOpts := &tele.SendOptions{DisableNotification: silent}
			if _, err := b.bot.Send(chat, chunk, plainOpts); err != nil {
				return fmt.Errorf("send message: %w", err)
			}
		}
	}
	return nil
}

// renderMarkdown converts standard markdown to Telegram MarkdownV2 format.
func renderMarkdown(md goldmarkMD, text string) string {
	var buf bytes.Buffer
	if err := md.Convert([]byte(text), &buf); err != nil {
		return text
	}
	result := buf.String()
	if result == "" {
		return text
	}
	return result
}

// splitMessage splits a message into chunks that fit within Telegram's 4096
// character limit. It tries to split at newline boundaries when possible.
func splitMessage(text string) []string {
	if len(text) <= telegramMaxMessageLen {
		return []string{text}
	}

	var chunks []string
	for len(text) > 0 {
		if len(text) <= telegramMaxMessageLen {
			chunks = append(chunks, text)
			break
		}

		cutAt := telegramMaxMessageLen
		// Try to find the last newline before the limit.
		if idx := strings.LastIndex(text[:cutAt], "\n"); idx > 0 {
			cutAt = idx + 1 // Include the newline in the current chunk.
		}

		chunks = append(chunks, text[:cutAt])
		text = text[cutAt:]
	}

	return chunks
}
