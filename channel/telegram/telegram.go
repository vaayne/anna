package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	tgmd "github.com/Mad-Pixels/goldmark-tgmd"

	"github.com/vaayne/anna/agent"
	"github.com/vaayne/anna/channel"
	tele "gopkg.in/telebot.v4"
)

const telegramMaxMessageLen = 4000

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
