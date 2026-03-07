package telegram

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vaayne/anna/agent/runner"
	tele "gopkg.in/telebot.v4"
)

// streamEditInterval controls how often we edit the message during streaming.
const streamEditInterval = time.Second

// typingInterval is how often we re-send the typing indicator. Telegram
// expires typing status after ~5 seconds, so we resend every 4s.
const typingInterval = 4 * time.Second

// typingCursor is appended to the message while streaming to indicate activity.
const typingCursor = " \u258D"

// toolEmoji maps known tool names to display emoji.
var toolEmoji = map[string]string{
	"bash":    "⚡",
	"read":    "📖",
	"write":   "✏️",
	"edit":    "🔧",
	"search":  "🔍",
	"default": "🔧",
}

// toolLine returns a short status line for a tool-use event.
func toolLine(t *runner.ToolUseEvent) string {
	emoji, ok := toolEmoji[t.Tool]
	if !ok {
		emoji = toolEmoji["default"]
	}
	switch t.Status {
	case "running":
		input := t.Input
		if len(input) > 60 {
			input = input[:57] + "..."
		}
		if input != "" {
			return fmt.Sprintf("%s %s: %s", emoji, t.Tool, input)
		}
		return fmt.Sprintf("%s %s", emoji, t.Tool)
	case "error":
		return fmt.Sprintf("❌ %s failed", t.Tool)
	default:
		return ""
	}
}

// streamResponse consumes the agent stream, displaying progress in real time.
// For private chats it uses Telegram's sendMessageDraft API (Bot API 9.3+)
// for smooth animated streaming. For groups (where drafts aren't supported)
// it falls back to the edit-in-place approach.
func (b *Bot) streamResponse(c tele.Context, sessionID, prompt string) (string, error) {
	events := b.pool.Chat(b.ctx, sessionID, prompt)

	if !isGroup(c) {
		text, fallback, err := b.streamDraft(c, events)
		if fallback {
			// Draft failed on first attempt — the event channel is still
			// open. Continue with edit-based streaming, preserving any
			// text already buffered from consumed events.
			logger().Info("sendMessageDraft not supported, falling back to edit mode")
			return b.streamEditEvents(c, events, text)
		}
		return text, err
	}
	return b.streamEditEvents(c, events, "")
}

// streamDraft uses Telegram's sendMessageDraft API for smooth streaming
// in private chats. If the first draft call fails, it returns fallback=true
// so the caller can switch to edit mode. The buffered text is returned so
// no consumed events are lost.
func (b *Bot) streamDraft(c tele.Context, events <-chan runner.Event) (text string, fallback bool, err error) {
	var sb strings.Builder
	var streamErr error
	var currentTool string
	lastSend := time.Time{}
	draftID := rand.Int64N(1<<53) + 1
	chatID := c.Chat().ID
	firstDraft := true

	for evt := range events {
		if evt.Err != nil {
			streamErr = evt.Err
			break
		}

		if evt.ToolUse != nil {
			line := toolLine(evt.ToolUse)
			if line != "" {
				currentTool = line
			} else {
				currentTool = ""
			}
			lastSend = time.Time{}
		}

		sb.WriteString(evt.Text)

		now := time.Now()
		if now.Sub(lastSend) < streamEditInterval {
			continue
		}

		current := sb.String()
		if strings.TrimSpace(current) == "" && currentTool == "" {
			continue
		}

		display := buildStreamDisplay(current, currentTool)

		if err := b.sendDraftRaw(chatID, draftID, display); err != nil {
			if firstDraft {
				return sb.String(), true, nil
			}
			// Mid-stream failure: log and continue without updates
			// rather than breaking the stream.
			logger().Warn("sendMessageDraft failed mid-stream", "error", err)
		}
		firstDraft = false
		lastSend = now
	}

	return sb.String(), false, streamErr
}

// streamEditEvents uses the traditional edit-in-place approach for streaming,
// consuming from an existing event channel. Required for group chats where
// sendMessageDraft is not available. Any already-buffered text from a prior
// draft attempt is preserved via the initial parameter.
func (b *Bot) streamEditEvents(c tele.Context, events <-chan runner.Event, initial string) (string, error) {
	var sb strings.Builder
	sb.WriteString(initial)
	var sentMsg *tele.Message
	var streamErr error
	var currentTool string
	lastEdit := time.Time{}

	for evt := range events {
		if evt.Err != nil {
			streamErr = evt.Err
			break
		}

		if evt.ToolUse != nil {
			line := toolLine(evt.ToolUse)
			if line != "" {
				currentTool = line
			} else {
				currentTool = ""
			}
			lastEdit = time.Time{}
		}

		sb.WriteString(evt.Text)

		now := time.Now()
		if now.Sub(lastEdit) < streamEditInterval {
			continue
		}

		current := sb.String()
		if strings.TrimSpace(current) == "" && currentTool == "" {
			continue
		}

		display := buildStreamDisplay(current, currentTool)

		if sentMsg == nil {
			msg, err := b.bot.Send(c.Chat(), display)
			if err != nil {
				logger().Warn("stream send failed", "error", err)
			} else {
				sentMsg = msg
			}
		} else {
			if _, err := b.bot.Edit(sentMsg, display); err != nil {
				logger().Warn("stream edit failed", "error", err)
			}
		}
		lastEdit = now
	}

	// Clean up the streaming message so the caller can send the final version.
	if sentMsg != nil {
		if err := b.bot.Delete(sentMsg); err != nil {
			logger().Warn("delete streaming message failed", "error", err)
		}
	}

	return sb.String(), streamErr
}

// buildStreamDisplay constructs the streaming display text with tool indicator,
// cursor, and length truncation (UTF-8 safe).
func buildStreamDisplay(text, currentTool string) string {
	display := text
	suffix := typingCursor
	if currentTool != "" {
		suffix = "\n\n_" + currentTool + "_" + typingCursor
	}

	if len(suffix) >= telegramMaxMessageLen {
		suffix = typingCursor
	}

	if len(display)+len(suffix) > telegramMaxMessageLen {
		cutAt := telegramMaxMessageLen - len(suffix) - 3
		if cutAt < 0 {
			cutAt = 0
		}
		for cutAt > 0 && !utf8.RuneStart(display[cutAt]) {
			cutAt--
		}
		display = display[:cutAt] + "..."
	}

	return display + suffix
}

// sendDraftRaw calls the Telegram sendMessageDraft API (Bot API 9.3+).
// This provides smooth animated streaming in private chats without
// the rate-limiting issues of repeated editMessageText calls.
func (b *Bot) sendDraftRaw(chatID, draftID int64, text string) error {
	params := map[string]string{
		"chat_id":  strconv.FormatInt(chatID, 10),
		"draft_id": strconv.FormatInt(draftID, 10),
		"text":     text,
	}
	_, err := b.bot.Raw("sendMessageDraft", params)
	return err
}

// keepTyping sends the typing indicator repeatedly until ctx is cancelled.
func keepTyping(ctx context.Context, c tele.Context) {
	_ = c.Notify(tele.Typing)
	ticker := time.NewTicker(typingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = c.Notify(tele.Typing)
		}
	}
}
