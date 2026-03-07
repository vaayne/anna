package telegram

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/vaayne/anna/channel"
	tele "gopkg.in/telebot.v4"
)

// ModelOption re-exports channel.ModelOption for use by callers.
type ModelOption = channel.ModelOption

// ModelListFunc re-exports channel.ModelListFunc for use by callers.
type ModelListFunc = channel.ModelListFunc

// ModelSwitchFunc re-exports channel.ModelSwitchFunc for use by callers.
type ModelSwitchFunc = channel.ModelSwitchFunc

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
