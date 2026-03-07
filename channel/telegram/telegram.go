package telegram

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
type ModelSwitchFunc = channel.ModelSwitchFunc

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

// logger returns the package logger, always using the current default handler.
// This must be a function (not a package-level var) because the default handler
// is set in main() after package init.
func logger() *slog.Logger { return slog.With("component", "telegram") }

// Config holds Telegram bot settings.
type Config struct {
	Token      string  // bot token
	NotifyChat string  // default chat ID for proactive notifications
	ChannelID  string  // broadcast channel ID or @username
	GroupMode  string  // "mention" | "always" | "disabled"
	AllowedIDs []int64 // user IDs allowed to use the bot (empty = allow all)
}

// Bot wraps a Telegram bot with agent pool integration.
type Bot struct {
	bot      *tele.Bot
	pool     *agent.Pool
	listFn   ModelListFunc
	switchFn ModelSwitchFunc
	md       goldmarkMD

	mu         sync.RWMutex
	chatModels map[int64]ModelOption

	allowed map[int64]struct{} // empty map = allow all
	cfg     Config
	ctx     context.Context
}

// New creates a Telegram bot and registers handlers. Call Start to begin polling.
func New(cfg Config, pool *agent.Pool, listFn ModelListFunc, switchFn ModelSwitchFunc) (*Bot, error) {
	bot, err := tele.NewBot(tele.Settings{
		Token: cfg.Token,
		Poller: &tele.LongPoller{
			Timeout:        30 * time.Second,
			AllowedUpdates: tele.AllowedUpdates,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create bot: %w", err)
	}

	if cfg.GroupMode == "" {
		cfg.GroupMode = "mention"
	}

	allowed := make(map[int64]struct{}, len(cfg.AllowedIDs))
	for _, id := range cfg.AllowedIDs {
		allowed[id] = struct{}{}
	}

	b := &Bot{
		bot:        bot,
		pool:       pool,
		listFn:     listFn,
		switchFn:   switchFn,
		md:         tgmd.TGMD(),
		chatModels: make(map[int64]ModelOption),
		allowed:    allowed,
		cfg:        cfg,
	}

	b.registerHandlers()
	return b, nil
}

// Start begins long polling. It blocks until ctx is cancelled.
func (b *Bot) Start(ctx context.Context) error {
	b.ctx = ctx

	if err := registerCommands(b.bot); err != nil {
		logger().Warn("register telegram commands failed", "error", err)
	}

	logger().Info("polling started")

	go func() {
		<-ctx.Done()
		logger().Info("polling stopped")
		b.bot.Stop()
	}()

	b.bot.Start()
	return ctx.Err()
}

// Name returns the backend name. Implements channel.Backend.
func (b *Bot) Name() string { return "telegram" }

// Notify sends a message to the specified chat. Implements channel.Backend.
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

	logger().Debug("sending notification", "chat_id", chatID, "text_len", len(n.Text), "silent", n.Silent)

	opts := &tele.SendOptions{ParseMode: tele.ModeMarkdownV2}
	if n.Silent {
		opts.DisableNotification = true
	}

	if err := b.sendChunkedMarkdown(chat, n.Text, n.Silent, opts); err != nil {
		return fmt.Errorf("send notification: %w", err)
	}
	logger().Debug("notification sent successfully", "chat_id", chatID)
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
	b.bot.Handle("/start", b.guard(func(c tele.Context) error {
		return c.Send(welcomeMessage, tele.ModeMarkdown)
	}))

	b.bot.Handle("/new", b.guard(func(c tele.Context) error {
		sessionID := sessionIDFor(c)
		if err := b.pool.Reset(sessionID); err != nil {
			logger().Error("reset session failed", "session_id", sessionID, "error", err)
			return c.Send(fmt.Sprintf("Error creating new session: %v", err))
		}
		logger().Info("session reset", "session_id", sessionID)
		return c.Send("New session started.")
	}))

	b.bot.Handle("/compact", b.guard(func(c tele.Context) error {
		sessionID := sessionIDFor(c)
		_ = c.Notify(tele.Typing)
		summary, err := b.pool.CompactSession(b.ctx, sessionID)
		if err != nil {
			logger().Error("compact session failed", "session_id", sessionID, "error", err)
			return c.Send(fmt.Sprintf("Compaction failed: %v", err))
		}
		logger().Info("session compacted", "session_id", sessionID, "summary_len", len(summary))
		return c.Send("Session compacted.")
	}))

	b.bot.Handle("/model", b.guard(func(c tele.Context) error {
		args := strings.TrimSpace(c.Message().Payload)
		models := b.listFn()

		if args == "" {
			return b.sendModelKeyboard(c, indexModels(models))
		}

		// Numeric arg → direct switch by index.
		if _, err := strconv.Atoi(args); err == nil {
			return b.switchModel(c, models, args)
		}

		// Text arg → filter models by substring match, preserving global indices.
		filtered := filterModels(models, args)
		if len(filtered) == 0 {
			return c.Send(fmt.Sprintf("No models matching %q.", args))
		}
		return b.sendModelKeyboard(c, filtered)
	}))

	// Handle inline keyboard callbacks for model selection via unique handler.
	// telebot strips the "\fmodel_select|" prefix, so c.Data() = "1", "2", etc.
	b.bot.Handle("\fmodel_select", b.guard(func(c tele.Context) error {
		idxStr := c.Data()
		logger().Debug("model_select callback fired", "data", idxStr, "sender", c.Sender().ID, "chat", c.Chat().ID)
		models := b.listFn()
		if err := b.switchModel(c, models, idxStr); err != nil {
			logger().Error("model switch failed", "data", idxStr, "error", err)
			return err
		}
		_ = c.Respond()
		return c.Delete()
	}))

	// Handle pagination for model keyboard.
	// Callback data format: "page" or "page|filter_query".
	b.bot.Handle("\fmodel_page", b.guard(func(c tele.Context) error {
		data := c.Data()
		pageStr, query, _ := strings.Cut(data, "|")
		page, _ := strconv.Atoi(pageStr)

		allModels := b.listFn()
		models := filterModels(allModels, query)
		if err := b.sendModelPage(c, models, page, query, true); err != nil {
			logger().Error("model page failed", "page", page, "error", err)
			return err
		}
		return c.Respond()
	}))

	// No-op handler for the page counter button.
	b.bot.Handle("\fmodel_noop", func(c tele.Context) error {
		return c.Respond()
	})

	b.bot.Handle(tele.OnText, b.guard(func(c tele.Context) error {
		return b.handleText(c)
	}))

	// Debug: catch-all callback handler for unmatched callbacks.
	b.bot.Handle(tele.OnCallback, func(c tele.Context) error {
		cb := c.Callback()
		logger().Warn("unmatched callback", "data", cb.Data, "unique", cb.Unique)
		return c.Respond()
	})
}

// guard wraps a handler with access control and group mode checks.
func (b *Bot) guard(h tele.HandlerFunc) tele.HandlerFunc {
	return func(c tele.Context) error {
		if !b.isAllowed(c) {
			if s := c.Sender(); s != nil {
				logger().Warn("unauthorized access", "user_id", s.ID)
			}
			return nil
		}
		// Skip group filtering for callback queries — they originate from
		// the bot's own inline keyboards (e.g. model selection) and don't
		// carry mention/reply context.
		if isGroup(c) && c.Callback() == nil && !b.shouldRespondInGroup(c) {
			logger().Debug("guard: skipped group message", "chat", c.Chat().ID)
			return nil
		}
		if c.Callback() != nil {
			logger().Debug("guard: passing callback through", "data", c.Callback().Data, "unique", c.Callback().Unique)
		}
		return h(c)
	}
}

// isAllowed returns true if the sender is in the allowed list.
// An empty allowed list means everyone is allowed.
func (b *Bot) isAllowed(c tele.Context) bool {
	if len(b.allowed) == 0 {
		return true
	}
	if c.Sender() == nil {
		return false
	}
	_, ok := b.allowed[c.Sender().ID]
	return ok
}

// handleText processes incoming text messages.
func (b *Bot) handleText(c tele.Context) error {
	chatID := c.Chat().ID
	sessionID := sessionIDFor(c)
	text := c.Message().Text

	// Strip bot mention in group chats (access control already handled by guard).
	if isGroup(c) {
		text = b.stripBotMention(text)
	}

	logger().Debug("message received", "chat_id", chatID, "text_len", len(text))

	typingCtx, stopTyping := context.WithCancel(b.ctx)
	go keepTyping(typingCtx, c)

	response, streamErr := b.streamResponse(c, sessionID, text)

	stopTyping()

	if streamErr != nil {
		logger().Error("agent stream error", "session_id", sessionID, "error", streamErr)
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
	logger().Debug("response sent", "chat_id", chatID, "response_len", len(response))
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

const modelsPerPage = 8

// indexedModel pairs a ModelOption with its 1-based index in the full model list.
type indexedModel struct {
	ModelOption
	globalIdx int
}

// indexModels wraps a full model list with sequential 1-based indices.
func indexModels(models []ModelOption) []indexedModel {
	out := make([]indexedModel, len(models))
	for i, m := range models {
		out[i] = indexedModel{ModelOption: m, globalIdx: i + 1}
	}
	return out
}

// filterModels returns indexed models matching the query (or all if query is empty).
func filterModels(models []ModelOption, query string) []indexedModel {
	if query == "" {
		return indexModels(models)
	}
	query = strings.ToLower(query)
	var out []indexedModel
	for i, m := range models {
		label := strings.ToLower(m.Provider + "/" + m.Model)
		if strings.Contains(label, query) {
			out = append(out, indexedModel{ModelOption: m, globalIdx: i + 1})
		}
	}
	return out
}

// sendModelKeyboard sends a paginated inline keyboard with model selection buttons.
func (b *Bot) sendModelKeyboard(c tele.Context, models []indexedModel) error {
	return b.sendModelPage(c, models, 0, "", false)
}

// sendModelPage renders a single page of the model keyboard.
// query is preserved in nav button data so filtered pagination works across pages.
// If edit is true, it edits the existing message instead of sending a new one.
func (b *Bot) sendModelPage(c tele.Context, models []indexedModel, page int, query string, edit bool) error {
	if len(models) == 0 {
		return c.Send("No models configured.")
	}

	totalPages := (len(models) + modelsPerPage - 1) / modelsPerPage
	if page < 0 {
		page = 0
	}
	if page >= totalPages {
		page = totalPages - 1
	}

	start := page * modelsPerPage
	end := start + modelsPerPage
	if end > len(models) {
		end = len(models)
	}

	b.mu.RLock()
	active, hasActive := b.chatModels[c.Chat().ID]
	b.mu.RUnlock()
	markup := &tele.ReplyMarkup{}

	var rows []tele.Row
	for i := start; i < end; i++ {
		m := models[i]
		label := fmt.Sprintf("%s/%s", m.Provider, m.Model)
		if hasActive && m.Provider == active.Provider && m.Model == active.Model {
			label = "✅ " + label
		}
		btn := markup.Data(label, "model_select", fmt.Sprintf("%d", m.globalIdx))
		rows = append(rows, markup.Row(btn))
	}

	// Navigation row. Encode "page|query" so filtered pagination persists.
	if totalPages > 1 {
		pageData := func(p int) string {
			if query == "" {
				return fmt.Sprintf("%d", p)
			}
			return fmt.Sprintf("%d|%s", p, query)
		}
		var navBtns []tele.Btn
		if page > 0 {
			navBtns = append(navBtns, markup.Data("◀ Prev", "model_page", pageData(page-1)))
		}
		navBtns = append(navBtns, markup.Data(fmt.Sprintf("%d/%d", page+1, totalPages), "model_noop"))
		if page < totalPages-1 {
			navBtns = append(navBtns, markup.Data("Next ▶", "model_page", pageData(page+1)))
		}
		rows = append(rows, markup.Row(navBtns...))
	}

	markup.Inline(rows...)

	text := fmt.Sprintf("Select a model (%d total):", len(models))
	if edit {
		return c.Edit(text, markup)
	}
	return c.Send(text, markup)
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
		logger().Error("reset session after model switch failed", "session_id", sessionID, "error", err)
	}
	b.mu.Lock()
	b.chatModels[c.Chat().ID] = selected
	b.mu.Unlock()
	logger().Info("model switched", "session_id", sessionID, "provider", selected.Provider, "model", selected.Model)
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
