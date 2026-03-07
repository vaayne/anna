package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tele "gopkg.in/telebot.v4"
)

const welcomeMessage = `👋 Hi! I'm Anna — your local AI assistant.

*Commands*
/new — Start a fresh session
/compact — Compress conversation history
/model — Switch between models

Just send me a message to get started.`

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
