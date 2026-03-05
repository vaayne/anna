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

// streamEditInterval controls how often we edit the message during streaming.
const streamEditInterval = time.Second

// typingCursor is appended to the message while streaming to indicate activity.
const typingCursor = " \u258D"

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

		response, streamErr := streamResponse(bot, c, ag, ctx, text)

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

		sendFinalResponse(bot, c, md, response)
		log.Debug("response sent", "chat_id", chatID, "response_len", len(response))
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

// streamResponse consumes the agent stream, sending and editing a Telegram
// message in place as tokens arrive. It returns the final accumulated text
// and any stream error. The sent message (if any) is deleted before returning
// so the caller can send the final rendered version.
func streamResponse(bot *tele.Bot, c tele.Context, ag *agent.Agent, ctx context.Context, prompt string) (string, error) {
	var sb strings.Builder
	var sentMsg *tele.Message
	var streamErr error
	lastEdit := time.Time{}

	for evt := range ag.SendPrompt(ctx, prompt) {
		if evt.Err != nil {
			streamErr = evt.Err
			break
		}
		sb.WriteString(evt.Text)

		now := time.Now()
		if now.Sub(lastEdit) < streamEditInterval {
			continue
		}

		current := sb.String()
		if strings.TrimSpace(current) == "" {
			continue
		}

		// Truncate display text if it exceeds the message limit.
		display := current
		if len(display)+len(typingCursor) > telegramMaxMessageLen {
			display = display[:telegramMaxMessageLen-len(typingCursor)-3] + "..."
		}

		if sentMsg == nil {
			msg, err := bot.Send(c.Chat(), display+typingCursor)
			if err != nil {
				log.Warn("stream send failed", "error", err)
			} else {
				sentMsg = msg
			}
		} else {
			if _, err := bot.Edit(sentMsg, display+typingCursor); err != nil {
				log.Warn("stream edit failed", "error", err)
			}
		}
		lastEdit = now
	}

	// Clean up the streaming message so the caller can send the final version.
	if sentMsg != nil {
		if err := bot.Delete(sentMsg); err != nil {
			log.Warn("delete streaming message failed", "error", err)
		}
	}

	return sb.String(), streamErr
}

// sendFinalResponse sends the completed response with markdown rendering,
// splitting into chunks if necessary.
func sendFinalResponse(bot *tele.Bot, c tele.Context, md goldmarkMD, response string) {
	chatID := c.Chat().ID
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
