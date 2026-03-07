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
	"github.com/vaayne/anna/agent/runner"
	"github.com/vaayne/anna/channel"
	tele "gopkg.in/telebot.v4"
)

// ModelOption re-exports channel.ModelOption for use by callers.
type ModelOption = channel.ModelOption

// ModelListFunc re-exports channel.ModelListFunc for use by callers.
type ModelListFunc = channel.ModelListFunc

// ModelSwitchFunc re-exports channel.ModelSwitchFunc for use by callers.

const telegramMaxMessageLen = 4000

// streamEditInterval controls how often we edit the message during streaming.
const streamEditInterval = time.Second

// typingInterval is how often we re-send the typing indicator. Telegram
// expires typing status after ~5 seconds, so we resend every 4s.
const typingInterval = 4 * time.Second

// typingCursor is appended to the message while streaming to indicate activity.
const typingCursor = " \u258D"

// callbackModelPrefix is the prefix for model-selection callback data.
const callbackModelPrefix = "model:"

// toolEmoji maps known tool names to display emoji.
var toolEmoji = map[string]string{
	"bash":    "⚡",
	"read":    "📖",
	"write":   "✏️",
	"edit":    "🔧",
	"search":  "🔍",
	"default": "🔧",
}

var log = slog.With("component", "telegram")

func botCommands() []tele.Command {
	return []tele.Command{
		{Text: "start", Description: "Welcome & help"},
		{Text: "new", Description: "Start a new session"},
		{Text: "compact", Description: "Compact session history"},
		{Text: "model", Description: "List or switch models"},
	}
}

func registerCommands(bot *tele.Bot) error {
	return bot.SetCommands(botCommands())
}

const welcomeMessage = `👋 Hi! I'm Anna — your local AI assistant.

*Commands*
/new — Start a fresh session
/compact — Compress conversation history
/model — Switch between models

Just send me a message to get started.`

// Run starts a Telegram bot using long polling. It blocks until ctx is
// cancelled.
func Run(ctx context.Context, token string, pool *agent.Pool, listFn ModelListFunc, switchFn channel.ModelSwitchFunc) error {
	// Track per-chat active model for display purposes.
	chatModels := make(map[int64]ModelOption)
	bot, err := tele.NewBot(tele.Settings{
		Token:  token,
		Poller: &tele.LongPoller{Timeout: 30 * time.Second},
	})
	if err != nil {
		return fmt.Errorf("create bot: %w", err)
	}

	md := tgmd.TGMD()

	if err := registerCommands(bot); err != nil {
		log.Warn("register telegram commands failed", "error", err)
	}

	bot.Handle("/start", func(c tele.Context) error {
		return c.Send(welcomeMessage, tele.ModeMarkdown)
	})

	bot.Handle("/new", func(c tele.Context) error {
		sessionID := strconv.FormatInt(c.Chat().ID, 10)
		if err := pool.Reset(sessionID); err != nil {
			log.Error("reset session failed", "session_id", sessionID, "error", err)
			return c.Send(fmt.Sprintf("Error creating new session: %v", err))
		}
		log.Info("session reset", "session_id", sessionID)
		return c.Send("New session started.")
	})

	bot.Handle("/compact", func(c tele.Context) error {
		sessionID := strconv.FormatInt(c.Chat().ID, 10)
		_ = c.Notify(tele.Typing)
		summary, err := pool.CompactSession(ctx, sessionID)
		if err != nil {
			log.Error("compact session failed", "session_id", sessionID, "error", err)
			return c.Send(fmt.Sprintf("Compaction failed: %v", err))
		}
		log.Info("session compacted", "session_id", sessionID, "summary_len", len(summary))
		return c.Send("Session compacted.")
	})

	bot.Handle("/model", func(c tele.Context) error {
		args := strings.TrimSpace(c.Message().Payload)
		models := listFn()

		// No argument: show inline keyboard.
		if args == "" {
			return sendModelKeyboard(c, models, chatModels)
		}

		// Argument provided: switch to that model.
		return switchModel(c, pool, chatModels, models, switchFn, args)
	})

	// Handle inline keyboard callbacks for model selection.
	bot.Handle(tele.OnCallback, func(c tele.Context) error {
		data := c.Data()
		if !strings.HasPrefix(data, callbackModelPrefix) {
			return c.Respond()
		}

		idxStr := strings.TrimPrefix(data, callbackModelPrefix)
		models := listFn()
		if err := switchModel(c, pool, chatModels, models, switchFn, idxStr); err != nil {
			return err
		}

		// Acknowledge the callback and remove the keyboard.
		_ = c.Respond()
		return c.Delete()
	})

	bot.Handle(tele.OnText, func(c tele.Context) error {
		chatID := c.Chat().ID
		sessionID := strconv.FormatInt(chatID, 10)
		text := c.Message().Text

		log.Debug("message received", "chat_id", chatID, "text_len", len(text))

		// Start persistent typing indicator.
		typingCtx, stopTyping := context.WithCancel(ctx)
		go keepTyping(typingCtx, c)

		response, streamErr := streamResponse(bot, c, pool, ctx, sessionID, text)

		stopTyping()

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

// sendModelKeyboard sends an inline keyboard with model selection buttons.
func sendModelKeyboard(c tele.Context, models []ModelOption, chatModels map[int64]ModelOption) error {
	if len(models) == 0 {
		return c.Send("No models configured.")
	}

	active, hasActive := chatModels[c.Chat().ID]
	markup := &tele.ReplyMarkup{}

	var rows []tele.Row
	for i, m := range models {
		label := fmt.Sprintf("%s/%s", m.Provider, m.Model)
		if hasActive && m.Provider == active.Provider && m.Model == active.Model {
			label = "✅ " + label
		}
		btn := markup.Data(label, "model_select", fmt.Sprintf("model:%d", i+1))
		rows = append(rows, markup.Row(btn))
	}

	markup.Inline(rows...)
	return c.Send("Select a model:", markup)
}

// switchModel handles model switching by index string. Used by both /model
// command and inline keyboard callback.
func switchModel(c tele.Context, pool *agent.Pool, chatModels map[int64]ModelOption, models []ModelOption, switchFn channel.ModelSwitchFunc, idxStr string) error {
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 1 || idx > len(models) {
		return c.Send(fmt.Sprintf("Invalid selection. Use a number between 1 and %d.", len(models)))
	}
	selected := models[idx-1]
	if switchFn != nil {
		if err := switchFn(selected.Provider, selected.Model); err != nil {
			return c.Send(fmt.Sprintf("Error switching model: %v", err))
		}
	}
	sessionID := strconv.FormatInt(c.Chat().ID, 10)
	if err := pool.Reset(sessionID); err != nil {
		log.Error("reset session after model switch failed", "session_id", sessionID, "error", err)
	}
	chatModels[c.Chat().ID] = selected
	log.Info("model switched", "session_id", sessionID, "provider", selected.Provider, "model", selected.Model)
	return c.Send(fmt.Sprintf("Switched to %s/%s. Session reset.", selected.Provider, selected.Model))
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

// streamResponse consumes the agent stream, sending and editing a Telegram
// message in place as tokens arrive. It returns the final accumulated text
// and any stream error. The sent message (if any) is deleted before returning
// so the caller can send the final rendered version.
func streamResponse(bot *tele.Bot, c tele.Context, pool *agent.Pool, ctx context.Context, sessionID, prompt string) (string, error) {
	var sb strings.Builder
	var sentMsg *tele.Message
	var streamErr error
	var currentTool string
	lastEdit := time.Time{}

	for evt := range pool.Chat(ctx, sessionID, prompt) {
		if evt.Err != nil {
			streamErr = evt.Err
			break
		}

		// Track tool-use events for display.
		if evt.ToolUse != nil {
			line := toolLine(evt.ToolUse)
			if line != "" {
				currentTool = line
			} else {
				currentTool = ""
			}
			// Force an immediate update to show tool status.
			lastEdit = time.Time{}
		}

		sb.WriteString(evt.Text)

		now := time.Now()
		if now.Sub(lastEdit) < streamEditInterval {
			continue
		}

		current := sb.String()
		// Show tool status even if no text yet.
		if strings.TrimSpace(current) == "" && currentTool == "" {
			continue
		}

		// Build display: text + tool indicator + cursor.
		display := current
		suffix := typingCursor
		if currentTool != "" {
			suffix = "\n\n_" + currentTool + "_" + typingCursor
		}

		if len(display)+len(suffix) > telegramMaxMessageLen {
			display = display[:telegramMaxMessageLen-len(suffix)-3] + "..."
		}

		if sentMsg == nil {
			msg, err := bot.Send(c.Chat(), display+suffix)
			if err != nil {
				log.Warn("stream send failed", "error", err)
			} else {
				sentMsg = msg
			}
		} else {
			if _, err := bot.Edit(sentMsg, display+suffix); err != nil {
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
