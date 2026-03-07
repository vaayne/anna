package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/vaayne/anna/agent"
	"github.com/vaayne/anna/agent/runner"
	"github.com/vaayne/anna/channel"
)

// streamStartMsg carries the stream channel from the agent.
type streamStartMsg struct {
	stream <-chan runner.Event
}

// streamChunkMsg carries a text delta from the agent stream.
type streamChunkMsg string

// streamToolMsg carries a tool-use event from the agent stream.
type streamToolMsg struct {
	tool   string
	status string
	input  string
	detail string
}

// streamDoneMsg signals the stream has finished.
type streamDoneMsg struct{}

// streamErrMsg carries a streaming error.
type streamErrMsg struct{ err error }

// compactDoneMsg signals that session compaction finished.
type compactDoneMsg struct {
	summary string
	err     error
}

type chatModel struct {
	ctx      context.Context
	pool     *agent.Pool
	textarea textarea.Model
	viewport viewport.Model
	stream   <-chan runner.Event

	sessionID   string
	provider    string
	model       string
	history     *strings.Builder
	streaming   bool
	status      string
	width       int
	height      int
	ready       bool
	switchModel channel.ModelSwitchFunc
	listModels  channel.ModelListFunc

	// Slash command completion
	completing     bool
	completions    []slashCommand
	completeCursor int

	// Tool use tracking
	toolStartTime time.Time

	// Markdown rendering: track current response segments
	historyPrefix string           // rendered history before current response
	currentRaw    *strings.Builder // raw markdown text of current streaming segment
	mdRenderer    *glamour.TermRenderer

	// Model picker
	picking        bool
	models         []modelOption
	filteredModels []modelOption
	modelCursor    int
	modelFilter    string
}

func newChatModel(ctx context.Context, pool *agent.Pool, provider, model string, listFn channel.ModelListFunc, switchFn channel.ModelSwitchFunc) chatModel {
	ta := textarea.New()
	ta.Placeholder = "Type a message... (Enter to send, Alt+Enter for newline)"
	ta.Focus()
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.Prompt = ""
	ta.SetHeight(1)

	// Resolve session: resume the most recent active session, or create a new one.
	sessionID := resolveSession(pool)

	m := chatModel{
		ctx:         ctx,
		pool:        pool,
		textarea:    ta,
		sessionID:   sessionID,
		provider:    provider,
		model:       model,
		history:     &strings.Builder{},
		listModels:  listFn,
		switchModel: switchFn,
		currentRaw:  &strings.Builder{},
	}

	// Restore conversation display from persisted history.
	if rendered := renderResumedHistory(pool, sessionID); rendered != "" {
		m.history.WriteString(rendered)
		m.historyPrefix = m.history.String()
	}

	return m
}

// resolveSession returns the most recently active non-archived session ID,
// or creates a new session if none exist.
func resolveSession(pool *agent.Pool) string {
	sessions, err := pool.ListSessions(false)
	if err == nil && len(sessions) > 0 {
		// Find the most recently active session.
		best := sessions[0]
		for _, s := range sessions[1:] {
			if s.LastActive.After(best.LastActive) {
				best = s
			}
		}
		return best.ID
	}

	info, err := pool.CreateSession()
	if err != nil {
		// Fallback: generate a simple ID so the app can still start.
		return "session"
	}
	return info.ID
}

// renderResumedHistory builds the viewport content from a session's persisted events.
// Only user messages and assistant text are rendered; tool calls are omitted for brevity.
func renderResumedHistory(pool *agent.Pool, sessionID string) string {
	events := pool.History(sessionID)
	if len(events) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(systemStyle.Render("[session resumed]") + "\n\n")

	for _, evt := range events {
		switch evt.Type {
		case runner.RPCEventUserMessage:
			if evt.Summary == "" || evt.Summary == "[Previous conversation summary]" {
				continue
			}
			b.WriteString(userStyle.Render("You") + "\n")
			b.WriteString(userBorderStyle.Render(chatTextStyle.Render(evt.Summary)) + "\n\n")

		case runner.RPCEventMessageUpdate:
			if evt.Summary == "" {
				continue
			}
			b.WriteString(agentStyle.Render("Anna") + "\n")
			b.WriteString(agentBorderStyle.Render(evt.Summary) + "\n\n")
		}
	}

	return b.String()
}

func (m chatModel) Init() tea.Cmd {
	return textarea.Blink
}

func (m chatModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		if m.picking {
			return m.handlePickingKey(msg)
		}

		switch msg.Type {
		case tea.KeyCtrlC:
			return m, tea.Quit

		case tea.KeyTab:
			if m.completing && len(m.completions) > 0 {
				selected := m.completions[m.completeCursor]
				m.textarea.SetValue(selected.name)
				m.completing = false
				m.completions = nil
				m.resize()
				return m, nil
			}

		case tea.KeyUp:
			if m.completing && len(m.completions) > 0 {
				if m.completeCursor > 0 {
					m.completeCursor--
				}
				return m, nil
			}

		case tea.KeyDown:
			if m.completing && len(m.completions) > 0 {
				if m.completeCursor < len(m.completions)-1 {
					m.completeCursor++
				}
				return m, nil
			}

		case tea.KeyEsc:
			if m.completing {
				m.completing = false
				m.completions = nil
				m.resize()
				return m, nil
			}

		case tea.KeyEnter:
			if m.streaming {
				break
			}
			// If completing, accept and submit the selected command.
			if m.completing && len(m.completions) > 0 {
				selected := m.completions[m.completeCursor]
				m.textarea.Reset()
				m.completing = false
				m.completions = nil
				m.resize()
				cmd := m.handleInput(selected.name)
				return m, cmd
			}
			input := strings.TrimSpace(m.textarea.Value())
			if input == "" {
				break
			}
			m.textarea.Reset()
			m.completing = false
			m.completions = nil
			cmd := m.handleInput(input)
			return m, cmd
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resize()
		return m, nil

	case streamStartMsg:
		m.stream = msg.stream
		return m, waitNextChunk(m.stream)

	case streamChunkMsg:
		m.status = ""
		m.currentRaw.WriteString(string(msg))
		m.refreshViewport()
		return m, waitNextChunk(m.stream)

	case streamToolMsg:
		label := msg.tool
		if msg.input != "" {
			label += ": " + msg.input
		}
		switch msg.status {
		case "running":
			m.toolStartTime = time.Now()
			m.status = "Running " + label + "..."
		case "done":
			// Flush current markdown segment into prefix before tool line
			if m.currentRaw.Len() > 0 {
				m.historyPrefix += agentBorderStyle.Render(m.renderMarkdown(m.currentRaw.String())) + "\n"
				m.currentRaw.Reset()
			}
			elapsed := formatDuration(time.Since(m.toolStartTime))
			m.status = ""
			m.historyPrefix += toolDoneStyle.Render(fmt.Sprintf("    ✓ %s (%s)", label, elapsed)) + "\n"
		case "error":
			if m.currentRaw.Len() > 0 {
				m.historyPrefix += agentBorderStyle.Render(m.renderMarkdown(m.currentRaw.String())) + "\n"
				m.currentRaw.Reset()
			}
			elapsed := formatDuration(time.Since(m.toolStartTime))
			m.status = ""
			line := fmt.Sprintf("    ✗ %s (%s)", label, elapsed)
			if msg.detail != "" {
				line += " — " + msg.detail
			}
			m.historyPrefix += toolErrorStyle.Render(line) + "\n"
		}
		m.refreshViewport()
		return m, waitNextChunk(m.stream)

	case streamDoneMsg:
		m.streaming = false
		m.status = ""
		m.stream = nil
		// Finalize: flush remaining markdown into history with agent border
		if m.currentRaw.Len() > 0 {
			m.historyPrefix += agentBorderStyle.Render(m.renderMarkdown(m.currentRaw.String()))
			m.currentRaw.Reset()
		}
		m.historyPrefix += "\n\n"
		m.history.Reset()
		m.history.WriteString(m.historyPrefix)
		m.viewport.SetContent(m.history.String())
		m.viewport.GotoBottom()
		m.textarea.Focus()
		return m, nil

	case streamErrMsg:
		m.streaming = false
		m.status = ""
		m.stream = nil
		if m.currentRaw.Len() > 0 {
			m.historyPrefix += agentBorderStyle.Render(m.renderMarkdown(m.currentRaw.String()))
			m.currentRaw.Reset()
		}
		m.historyPrefix += "\n" + errorStyle.Render("error: "+msg.err.Error()) + "\n\n"
		m.history.Reset()
		m.history.WriteString(m.historyPrefix)
		m.viewport.SetContent(m.history.String())
		m.viewport.GotoBottom()
		m.textarea.Focus()
		return m, nil

	case compactDoneMsg:
		m.streaming = false
		m.status = ""
		if msg.err != nil {
			m.history.WriteString(errorStyle.Render("compaction failed: "+msg.err.Error()) + "\n\n")
		} else {
			m.history.WriteString(systemStyle.Render("[session compacted]") + "\n\n")
		}
		m.historyPrefix = m.history.String()
		m.viewport.SetContent(m.history.String())
		m.viewport.GotoBottom()
		m.textarea.Focus()
		return m, nil
	}

	if !m.streaming {
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		cmds = append(cmds, cmd)

		// Update completion state after textarea content changes.
		m.updateCompletions()
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

// updateCompletions shows/hides the command completion popup based on textarea content.
func (m *chatModel) updateCompletions() {
	val := m.textarea.Value()
	if strings.HasPrefix(val, "/") {
		matches := filterCommands(val)
		wasCompleting := m.completing
		m.completing = len(matches) > 0
		m.completions = matches
		if m.completeCursor >= len(matches) {
			m.completeCursor = 0
		}
		if m.completing != wasCompleting {
			m.resize()
		}
	} else if m.completing {
		m.completing = false
		m.completions = nil
		m.completeCursor = 0
		m.resize()
	}
}

// horizontal padding on each side of the content area
const padX = 1

func (m *chatModel) resize() {
	// Layout height budget (count actual rendered lines):
	//   header (title):             1 line
	//   blank line (\n\n):          1 line
	//   viewport:                   vpHeight lines
	//   input separator top:        1 line
	//   prompt + textarea:          ta.Height lines
	//   input separator bottom:     1 line
	//   help bar:                   1 line
	//   completion popup:           variable
	const chrome = 1 + 1 + 1 + 1 + 1 + 1 // header + blank + sep top + sep bottom + newline + help

	completionHeight := 0
	if m.completing && len(m.completions) > 0 {
		completionHeight = len(m.completions)
	}

	vpHeight := m.height - chrome - m.textarea.Height() - completionHeight
	if vpHeight < 1 {
		vpHeight = 1
	}

	innerWidth := m.width - padX*2

	if !m.ready {
		m.viewport = viewport.New(innerWidth, vpHeight)
		m.viewport.SetContent(m.history.String())
		m.ready = true
	} else {
		m.viewport.Width = innerWidth
		m.viewport.Height = vpHeight
	}

	m.textarea.SetWidth(innerWidth - 2) // subtract prompt "> " width

	// Recreate markdown renderer with no document margin for flush-left alignment
	style := styles.DarkStyleConfig
	if !termenv.HasDarkBackground() {
		style = styles.LightStyleConfig
	}
	style.Document.Margin = uintPtr(0)
	style.Document.BlockPrefix = ""
	style.Document.BlockSuffix = ""
	m.mdRenderer, _ = glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(innerWidth),
	)
}

func (m *chatModel) handleInput(input string) tea.Cmd {
	switch input {
	case "/quit", "/exit":
		return tea.Quit
	case "/new":
		// Archive current session and create a fresh one.
		_ = m.pool.ArchiveSession(m.sessionID)
		info, err := m.pool.CreateSession()
		if err != nil {
			m.history.WriteString(errorStyle.Render("error: "+err.Error()) + "\n\n")
		} else {
			m.sessionID = info.ID
			m.history.Reset()
			m.historyPrefix = ""
			m.history.WriteString(systemStyle.Render("[new session started]") + "\n\n")
		}
		m.viewport.SetContent(m.history.String())
		m.viewport.GotoBottom()
		return nil
	case "/compact":
		m.streaming = true
		m.status = "Compacting session..."
		m.textarea.Blur()
		ctx := m.ctx
		sessionID := m.sessionID
		return func() tea.Msg {
			summary, err := m.pool.CompactSession(ctx, sessionID)
			return compactDoneMsg{summary: summary, err: err}
		}
	case "/model":
		m.models = toModelOptions(m.listModels())
		if len(m.models) == 0 {
			m.history.WriteString(systemStyle.Render("[no models configured]") + "\n\n")
			m.viewport.SetContent(m.history.String())
			m.viewport.GotoBottom()
			return nil
		}
		m.picking = true
		m.modelFilter = ""
		m.filteredModels = m.models
		m.modelCursor = m.currentModelIndex()
		m.textarea.Blur()
		m.viewport.SetContent(renderModelPicker(m.filteredModels, m.modelCursor, m.provider, m.model, m.modelFilter))
		m.viewport.GotoTop()
		return nil
	}

	m.history.WriteString(userStyle.Render("You") + "\n" + userBorderStyle.Render(chatTextStyle.Render(input)) + "\n\n")
	m.history.WriteString(agentStyle.Render("Anna") + "\n")
	m.historyPrefix = m.history.String()
	m.currentRaw.Reset()
	m.viewport.SetContent(m.historyPrefix)
	m.viewport.GotoBottom()

	m.streaming = true
	m.status = "Thinking..."
	m.textarea.Blur()

	ctx := m.ctx
	sessionID := m.sessionID
	return func() tea.Msg {
		stream := m.pool.Chat(ctx, sessionID, input)
		return streamStartMsg{stream: stream}
	}
}

// waitNextChunk returns a Cmd that reads the next event from the stream channel.
func waitNextChunk(stream <-chan runner.Event) tea.Cmd {
	if stream == nil {
		return nil
	}
	return func() tea.Msg {
		evt, ok := <-stream
		if !ok {
			return streamDoneMsg{}
		}
		if evt.Err != nil {
			return streamErrMsg{evt.Err}
		}
		if evt.ToolUse != nil {
			return streamToolMsg{
				tool:   evt.ToolUse.Tool,
				status: evt.ToolUse.Status,
				input:  evt.ToolUse.Input,
				detail: evt.ToolUse.Detail,
			}
		}
		return streamChunkMsg(evt.Text)
	}
}

// renderMarkdown renders raw markdown text using glamour, falling back to raw text on error.
func (m *chatModel) renderMarkdown(raw string) string {
	if m.mdRenderer == nil || raw == "" {
		return raw
	}
	rendered, err := m.mdRenderer.Render(raw)
	if err != nil {
		return raw
	}
	return strings.TrimRight(rendered, "\n")
}

// refreshViewport rebuilds viewport content from historyPrefix + rendered current response.
func (m *chatModel) refreshViewport() {
	rendered := m.renderMarkdown(m.currentRaw.String())
	content := m.historyPrefix + agentBorderStyle.Render(rendered)
	m.viewport.SetContent(content)
	m.viewport.GotoBottom()
}

func uintPtr(v uint) *uint { return &v }

// padLines prepends padding to each line of text.
func padLines(text, pad string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = pad + line
	}
	return strings.Join(lines, "\n")
}

// formatDuration returns a human-friendly duration string.
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return fmt.Sprintf("%.0fm%.0fs", d.Minutes(), d.Seconds()-d.Minutes()*60)
	}
}

func (m chatModel) handlePickingKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC:
		return m, tea.Quit

	case tea.KeyEsc:
		m.picking = false
		m.viewport.SetContent(m.history.String())
		m.viewport.GotoBottom()
		m.textarea.Focus()
		return m, nil

	case tea.KeyUp:
		if m.modelCursor > 0 {
			m.modelCursor--
		}
		m.viewport.SetContent(renderModelPicker(m.filteredModels, m.modelCursor, m.provider, m.model, m.modelFilter))
		return m, nil

	case tea.KeyDown:
		if m.modelCursor < len(m.filteredModels)-1 {
			m.modelCursor++
		}
		m.viewport.SetContent(renderModelPicker(m.filteredModels, m.modelCursor, m.provider, m.model, m.modelFilter))
		return m, nil

	case tea.KeyEnter:
		if len(m.filteredModels) == 0 {
			return m, nil
		}
		selected := m.filteredModels[m.modelCursor]
		m.picking = false

		if selected.provider == m.provider && selected.model == m.model {
			m.viewport.SetContent(m.history.String())
			m.viewport.GotoBottom()
			m.textarea.Focus()
			return m, nil
		}

		if m.switchModel != nil {
			if err := m.switchModel(selected.provider, selected.model); err != nil {
				m.history.WriteString(errorStyle.Render("error switching model: "+err.Error()) + "\n\n")
				m.viewport.SetContent(m.history.String())
				m.viewport.GotoBottom()
				m.textarea.Focus()
				return m, nil
			}
		}

		if err := m.pool.Reset(m.sessionID); err != nil {
			m.history.WriteString(errorStyle.Render("error resetting session: "+err.Error()) + "\n\n")
		}

		m.provider = selected.provider
		m.model = selected.model
		m.history.WriteString(systemStyle.Render(fmt.Sprintf("[switched to %s/%s]", m.provider, m.model)) + "\n\n")
		m.viewport.SetContent(m.history.String())
		m.viewport.GotoBottom()
		m.textarea.Focus()
		return m, nil

	case tea.KeyBackspace:
		if len(m.modelFilter) > 0 {
			m.modelFilter = m.modelFilter[:len(m.modelFilter)-1]
			m.filteredModels = filterModels(m.models, m.modelFilter)
			m.modelCursor = 0
			m.viewport.SetContent(renderModelPicker(m.filteredModels, m.modelCursor, m.provider, m.model, m.modelFilter))
			m.viewport.GotoTop()
		}
		return m, nil

	case tea.KeyRunes:
		m.modelFilter += string(msg.Runes)
		m.filteredModels = filterModels(m.models, m.modelFilter)
		m.modelCursor = 0
		m.viewport.SetContent(renderModelPicker(m.filteredModels, m.modelCursor, m.provider, m.model, m.modelFilter))
		m.viewport.GotoTop()
		return m, nil
	}

	return m, nil
}

func (m chatModel) currentModelIndex() int {
	for i, opt := range m.filteredModels {
		if opt.provider == m.provider && opt.model == m.model {
			return i
		}
	}
	return 0
}

func (m chatModel) View() string {
	if !m.ready {
		return "Initializing..."
	}

	pad := strings.Repeat(" ", padX)

	// Title bar: "Anna" left, "provider/model" right
	title := titleStyle.Render("Anna")
	modelInfo := modelInfoStyle.Render(m.provider + "/" + m.model)
	titleGap := m.width - padX*2 - lipgloss.Width(title) - lipgloss.Width(modelInfo)
	if titleGap < 0 {
		titleGap = 0
	}
	header := pad + title + strings.Repeat(" ", titleGap) + modelInfo + pad

	// Input area — two thin separator lines with > prompt
	sepLine := pad + inputSeparator.Render(strings.Repeat("─", m.width-padX*2))
	prompt := inputPromptStyle.Render(">")
	input := sepLine + "\n" + pad + prompt + " " + m.textarea.View() + "\n" + sepLine

	// Completion popup
	completionView := ""
	if m.completing && len(m.completions) > 0 {
		completionView = "\n" + renderCompletions(m.completions, m.completeCursor)
	}

	// Help bar below input
	var helpText string
	switch {
	case m.picking:
		helpText = helpStyle.Render("↑↓ · enter · esc")
	case m.completing:
		helpText = helpStyle.Render("↑↓ · tab · enter · esc")
	default:
		helpText = helpAccentStyle.Render("/new") + helpStyle.Render(" · ") +
			helpAccentStyle.Render("/model") + helpStyle.Render(" · ") +
			helpAccentStyle.Render("/quit")
	}
	status := ""
	if m.status != "" {
		status = statusStyle.Render(m.status)
	}
	helpGap := m.width - padX*2 - lipgloss.Width(helpText) - lipgloss.Width(status)
	if helpGap < 0 {
		helpGap = 0
	}
	helpBar := pad + helpText + strings.Repeat(" ", helpGap) + status

	// Viewport with padding
	vpView := padLines(m.viewport.View(), pad)

	return header + "\n\n" + vpView + "\n" + input + completionView + "\n" + helpBar
}
