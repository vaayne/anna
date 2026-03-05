package telegram

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	tgmd "github.com/Mad-Pixels/goldmark-tgmd"
	"github.com/yuin/goldmark/parser"

	"github.com/vaayne/anna/agent"
	tele "gopkg.in/telebot.v4"
)

const telegramMaxMessageLen = 4000

var log = slog.With("component", "telegram")

// Run starts a Telegram bot using long polling. It blocks until ctx is
// cancelled.
func Run(ctx context.Context, token string, sm agent.SessionProvider) error {
	bot, err := tele.NewBot(tele.Settings{
		Token:  token,
		Poller: &tele.LongPoller{Timeout: 30 * time.Second},
	})
	if err != nil {
		return fmt.Errorf("create bot: %w", err)
	}

	md := tgmd.TGMD()

	bot.Handle("/new", func(c tele.Context) error {
		sessionID := strconv.FormatInt(c.Chat().ID, 10)
		if err := sm.NewSession(sessionID); err != nil {
			log.Error("new session failed", "session_id", sessionID, "error", err)
			return c.Send(fmt.Sprintf("Error creating new session: %v", err))
		}
		log.Info("new session created", "session_id", sessionID)
		return c.Send("New session started.")
	})

	bot.Handle(tele.OnText, func(c tele.Context) error {
		chatID := c.Chat().ID
		sessionID := strconv.FormatInt(chatID, 10)
		text := c.Message().Text

		log.Debug("message received", "chat_id", chatID, "text_len", len(text))

		_ = c.Notify(tele.Typing)

		ag, err := sm.GetOrCreate(ctx, sessionID)
		if err != nil {
			log.Error("get or create agent failed", "session_id", sessionID, "error", err)
			return c.Send(fmt.Sprintf("Error starting agent: %v", err))
		}

		var sb strings.Builder
		var streamErr error
		for evt := range ag.SendPrompt(ctx, text) {
			if evt.Err != nil {
				streamErr = evt.Err
				break
			}
			sb.WriteString(evt.Text)
		}

		response := sb.String()
		if streamErr != nil {
			log.Error("agent stream error", "session_id", sessionID, "error", streamErr)
			if response == "" {
				response = fmt.Sprintf("Agent error: %v", streamErr)
			} else {
				response += fmt.Sprintf("\n\n[Agent error: %v]", streamErr)
			}
		}

		if strings.TrimSpace(response) == "" {
			response = "(empty response)"
		}

		chunks := splitMessage(response)
		for _, chunk := range chunks {
			rendered := renderMarkdown(md, chunk)
			if err := c.Send(rendered, tele.ModeMarkdownV2); err != nil {
				log.Warn("markdown send failed, falling back to plain text", "error", err)
				if err := c.Send(chunk); err != nil {
					log.Error("sendMessage failed", "chat_id", chatID, "error", err)
				}
			}
		}
		log.Debug("response sent", "chat_id", chatID, "chunks", len(chunks), "response_len", len(response))
		return nil
	})

	log.Info("polling started")

	go func() {
		<-ctx.Done()
		log.Info("polling stopped")
		bot.Stop()
	}()

	bot.Start()
	return ctx.Err()
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

// goldmarkMD is the interface satisfied by the goldmark Markdown converter.
type goldmarkMD interface {
	Convert(source []byte, w io.Writer, opts ...parser.ParseOption) error
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
