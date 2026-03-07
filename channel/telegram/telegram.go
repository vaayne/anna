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

// Config holds Telegram bot settings.
type Config struct {
	Token      string // bot token
	NotifyChat string // default chat ID for proactive notifications
	ChannelID  string // broadcast channel ID or @username
	GroupMode  string // "mention" | "always" | "disabled"
}

// Bot wraps a Telegram bot with agent pool integration.
type Bot struct {
	bot        *tele.Bot
	pool       *agent.Pool
	listFn     ModelListFunc
	switchFn   channel.ModelSwitchFunc
	md         goldmarkMD
	chatModels map[int64]ModelOption
	cfg        Config
	ctx        context.Context
}

// New creates a Telegram bot and registers handlers. Call Start to begin polling.
func New(cfg Config, pool *agent.Pool, listFn ModelListFunc, switchFn channel.ModelSwitchFunc) (*Bot, error) {
	bot, err := tele.NewBot(tele.Settings{
		Token:  cfg.Token,
		Poller: &tele.LongPoller{Timeout: 30 * time.Second},
	})
	if err != nil {
		return nil, fmt.Errorf("create bot: %w", err)
	}

	if cfg.GroupMode == "" {
		cfg.GroupMode = "mention"
	}

	b := &Bot{
		bot:        bot,
		pool:       pool,
		listFn:     listFn,
		switchFn:   switchFn,
		md:         tgmd.TGMD(),
		chatModels: make(map[int64]ModelOption),
		cfg:        cfg,
	}

	b.registerHandlers()
	return b, nil
}

// Start begins long polling. It blocks until ctx is cancelled.
func (b *Bot) Start(ctx context.Context) error {
	b.ctx = ctx

	if err := registerCommands(b.bot); err != nil {
		log.Warn("register telegram commands failed", "error", err)
	}

	log.Info("polling started")

	go func() {
		<-ctx.Done()
		log.Info("polling stopped")
		b.bot.Stop()
	}()

	b.bot.Start()
	return ctx.Err()
}

// Notify sends a message to the specified chat. Implements channel.Notifier.
func (b *Bot) Notify(_ context.Context, n channel.Notification) error {
	chatID := n.ChatID
	if chatID == "" {
		chatID = b.cfg.NotifyChat
	}
	if chatID == "" {
		chatID = b.cfg.ChannelID
	}
	if chatID == "" {
		return fmt.Errorf("no target chat ID")
	}

	// Support both numeric IDs and @username for channels.
	var chat tele.Recipient
	if id, err := strconv.ParseInt(chatID, 10, 64); err == nil {
		chat = &tele.Chat{ID: id}
	} else {
		chat = chatRef(chatID)
	}

	opts := &tele.SendOptions{}
	if n.Silent {
		opts.DisableNotification = true
	}

	chunks := splitMessage(n.Text)
	for _, chunk := range chunks {
		rendered := renderMarkdown(b.md, chunk)
		if _, err := b.bot.Send(chat, rendered, tele.ModeMarkdownV2, opts); err != nil {
			// Fallback to plain text.
			if _, err := b.bot.Send(chat, chunk, opts); err != nil {
				return fmt.Errorf("send notification: %w", err)
			}
		}
	}
	return nil
}

// chatRef wraps a string (like "@channel_name") as a tele.Recipient.
type chatRef string

func (c chatRef) Recipient() string { return string(c) }

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

func (b *Bot) registerHandlers() {
	b.bot.Handle("/start", b.groupGuard(func(c tele.Context) error {
		return c.Send(welcomeMessage, tele.ModeMarkdown)
	}))

	b.bot.Handle("/new", b.groupGuard(func(c tele.Context) error {
		sessionID := sessionIDFor(c)
		if err := b.pool.Reset(sessionID); err != nil {
			log.Error("reset session failed", "session_id", sessionID, "error", err)
			return c.Send(fmt.Sprintf("Error creating new session: %v", err))
		}
		log.Info("session reset", "session_id", sessionID)
		return c.Send("New session started.")
	}))

	b.bot.Handle("/compact", b.groupGuard(func(c tele.Context) error {
		sessionID := sessionIDFor(c)
		_ = c.Notify(tele.Typing)
		summary, err := b.pool.CompactSession(b.ctx, sessionID)
		if err != nil {
			log.Error("compact session failed", "session_id", sessionID, "error", err)
			return c.Send(fmt.Sprintf("Compaction failed: %v", err))
		}
		log.Info("session compacted", "session_id", sessionID, "summary_len", len(summary))
		return c.Send("Session compacted.")
	}))

	b.bot.Handle("/model", b.groupGuard(func(c tele.Context) error {
		args := strings.TrimSpace(c.Message().Payload)
		models := b.listFn()

		if args == "" {
			return b.sendModelKeyboard(c, models)
		}
		return b.switchModel(c, models, args)
	}))

	// Handle inline keyboard callbacks for model selection via unique handler.
	// telebot strips the "\fmodel_select|" prefix, so c.Data() = "1", "2", etc.
	b.bot.Handle("\fmodel_select", func(c tele.Context) error {
		idxStr := c.Data()
		models := b.listFn()
		if err := b.switchModel(c, models, idxStr); err != nil {
			return err
		}
		_ = c.Respond()
		return c.Delete()
	})

	b.bot.Handle(tele.OnText, func(c tele.Context) error {
		return b.handleText(c)
	})
}

// groupGuard wraps a handler so it respects group_mode settings.
// In groups, the handler is skipped unless the bot should respond.
func (b *Bot) groupGuard(h tele.HandlerFunc) tele.HandlerFunc {
	return func(c tele.Context) error {
		if isGroup(c) && !b.shouldRespondInGroup(c) {
			return nil
		}
		return h(c)
	}
}

// handleText processes incoming text messages.
func (b *Bot) handleText(c tele.Context) error {
	chatID := c.Chat().ID
	sessionID := sessionIDFor(c)
	text := c.Message().Text

	// Group chat filtering.
	if isGroup(c) {
		if !b.shouldRespondInGroup(c) {
			return nil // silently ignore
		}
		// Strip bot mention from the message text.
		text = b.stripBotMention(text)
	}

	log.Debug("message received", "chat_id", chatID, "text_len", len(text))

	typingCtx, stopTyping := context.WithCancel(b.ctx)
	go keepTyping(typingCtx, c)

	response, streamErr := b.streamResponse(c, sessionID, text)

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

	b.sendFinalResponse(c, response)
	log.Debug("response sent", "chat_id", chatID, "response_len", len(response))
	return nil
}

// isGroup returns true if the message is from a group or supergroup.
func isGroup(c tele.Context) bool {
	t := c.Chat().Type
	return t == tele.ChatGroup || t == tele.ChatSuperGroup
}

// shouldRespondInGroup checks whether the bot should respond based on group_mode.
func (b *Bot) shouldRespondInGroup(c tele.Context) bool {
	switch b.cfg.GroupMode {
	case "disabled":
		return false
	case "always":
		return true
	default: // "mention"
		return b.isMentionedOrReplied(c)
	}
}

// isMentionedOrReplied returns true if the bot is @mentioned in the text
// or the message is a reply to one of the bot's messages.
func (b *Bot) isMentionedOrReplied(c tele.Context) bool {
	// Check for reply to bot.
	if reply := c.Message().ReplyTo; reply != nil && reply.Sender != nil {
		if reply.Sender.ID == b.bot.Me.ID {
			return true
		}
	}
	// Check for @mention.
	if b.bot.Me.Username != "" {
		if strings.Contains(c.Message().Text, "@"+b.bot.Me.Username) {
			return true
		}
	}
	return false
}

// stripBotMention removes @botname from the message text.
func (b *Bot) stripBotMention(text string) string {
	if b.bot.Me.Username == "" {
		return text
	}
	return strings.TrimSpace(strings.ReplaceAll(text, "@"+b.bot.Me.Username, ""))
}

// sessionIDFor returns the session ID for a chat. Uses chat ID directly
// so groups share a single session.
func sessionIDFor(c tele.Context) string {
	return strconv.FormatInt(c.Chat().ID, 10)
}

// sendModelKeyboard sends an inline keyboard with model selection buttons.
func (b *Bot) sendModelKeyboard(c tele.Context, models []ModelOption) error {
	if len(models) == 0 {
		return c.Send("No models configured.")
	}

	active, hasActive := b.chatModels[c.Chat().ID]
	markup := &tele.ReplyMarkup{}

	var rows []tele.Row
	for i, m := range models {
		label := fmt.Sprintf("%s/%s", m.Provider, m.Model)
		if hasActive && m.Provider == active.Provider && m.Model == active.Model {
			label = "✅ " + label
		}
		btn := markup.Data(label, "model_select", fmt.Sprintf("%d", i+1))
		rows = append(rows, markup.Row(btn))
	}

	markup.Inline(rows...)
	return c.Send("Select a model:", markup)
}

// switchModel handles model switching by index string.
func (b *Bot) switchModel(c tele.Context, models []ModelOption, idxStr string) error {
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 1 || idx > len(models) {
		return c.Send(fmt.Sprintf("Invalid selection. Use a number between 1 and %d.", len(models)))
	}
	selected := models[idx-1]
	if b.switchFn != nil {
		if err := b.switchFn(selected.Provider, selected.Model); err != nil {
			return c.Send(fmt.Sprintf("Error switching model: %v", err))
		}
	}
	sessionID := sessionIDFor(c)
	if err := b.pool.Reset(sessionID); err != nil {
		log.Error("reset session after model switch failed", "session_id", sessionID, "error", err)
	}
	b.chatModels[c.Chat().ID] = selected
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
func (b *Bot) streamResponse(c tele.Context, sessionID, prompt string) (string, error) {
	var sb strings.Builder
	var sentMsg *tele.Message
	var streamErr error
	var currentTool string
	lastEdit := time.Time{}

	for evt := range b.pool.Chat(b.ctx, sessionID, prompt) {
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

		// Guard against suffix being longer than the message limit.
		if len(suffix) >= telegramMaxMessageLen {
			suffix = typingCursor
		}

		if len(display)+len(suffix) > telegramMaxMessageLen {
			cutAt := telegramMaxMessageLen - len(suffix) - 3
			if cutAt < 0 {
				cutAt = 0
			}
			display = display[:cutAt] + "..."
		}

		if sentMsg == nil {
			msg, err := b.bot.Send(c.Chat(), display+suffix)
			if err != nil {
				log.Warn("stream send failed", "error", err)
			} else {
				sentMsg = msg
			}
		} else {
			if _, err := b.bot.Edit(sentMsg, display+suffix); err != nil {
				log.Warn("stream edit failed", "error", err)
			}
		}
		lastEdit = now
	}

	// Clean up the streaming message so the caller can send the final version.
	if sentMsg != nil {
		if err := b.bot.Delete(sentMsg); err != nil {
			log.Warn("delete streaming message failed", "error", err)
		}
	}

	return sb.String(), streamErr
}

// sendFinalResponse sends the completed response with markdown rendering,
// splitting into chunks if necessary.
func (b *Bot) sendFinalResponse(c tele.Context, response string) {
	chatID := c.Chat().ID
	chunks := splitMessage(response)
	for _, chunk := range chunks {
		rendered := renderMarkdown(b.md, chunk)
		if err := c.Send(rendered, tele.ModeMarkdownV2); err != nil {
			log.Warn("markdown send failed, falling back to plain text", "error", err)
			if err := c.Send(chunk); err != nil {
				log.Error("sendMessage failed", "chat_id", chatID, "error", err)
			}
		}
	}
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
