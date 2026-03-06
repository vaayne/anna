package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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

type chatModel struct {
	ctx      context.Context
	pool     *agent.Pool
	textarea textarea.Model
	viewport viewport.Model
	stream   <-chan runner.Event

	provider    string
	model       string
	history     *strings.Builder
	streaming   bool
	status      string
	width       int
	height      int
	ready       bool
	switchModel channel.ModelSwitchFunc

	// Slash command completion
	completing     bool
	completions    []slashCommand
	completeCursor int

	// Tool use tracking
	toolStartTime time.Time

	// Model picker
	picking bool
	models         []modelOption
	filteredModels []modelOption
	modelCursor    int
	modelFilter    string
}

func newChatModel(ctx context.Context, pool *agent.Pool, provider, model string, models []modelOption, switchFn channel.ModelSwitchFunc) chatModel {
	ta := textarea.New()
	ta.Placeholder = "Type a message... (Enter to send, Alt+Enter for newline)"
	ta.Focus()
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.SetHeight(3)

	return chatModel{
		ctx:         ctx,
		pool:        pool,
		textarea:    ta,
		provider:    provider,
		model:       model,
		history:     &strings.Builder{},
		models:      models,
		switchModel: switchFn,
	}
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
		m.history.WriteString(string(msg))
		m.viewport.SetContent(m.history.String())
		m.viewport.GotoBottom()
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
			elapsed := formatDuration(time.Since(m.toolStartTime))
			m.status = ""
			m.history.WriteString(toolDoneStyle.Render(fmt.Sprintf("    ✓ %s (%s)", label, elapsed)) + "\n")
		case "error":
			elapsed := formatDuration(time.Since(m.toolStartTime))
			m.status = ""
			line := fmt.Sprintf("    ✗ %s (%s)", label, elapsed)
			if msg.detail != "" {
				line += " — " + msg.detail
			}
			m.history.WriteString(toolErrorStyle.Render(line) + "\n")
		}
		m.viewport.SetContent(m.history.String())
		m.viewport.GotoBottom()
		return m, waitNextChunk(m.stream)

	case streamDoneMsg:
		m.streaming = false
		m.status = ""
		m.stream = nil
		m.history.WriteString("\n\n")
		m.viewport.SetContent(m.history.String())
		m.viewport.GotoBottom()
		m.textarea.Focus()
		return m, nil

	case streamErrMsg:
		m.streaming = false
		m.status = ""
		m.stream = nil
		m.history.WriteString("\n" + errorStyle.Render("error: "+msg.err.Error()) + "\n\n")
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

func (m *chatModel) resize() {
	// Layout height budget:
	// - Title bar: 1 line
	// - Separator: 1 line
	// - Viewport border: 2 lines (top + bottom)
	// - Input (textarea height + 2 for border)
	// - Completion popup: variable
	// - Help bar: 1 line
	titleHeight := 1
	separatorHeight := 1
	vpBorderHeight := 2
	inputHeight := m.textarea.Height() + 2 // textarea + border
	helpHeight := 1

	completionHeight := 0
	if m.completing && len(m.completions) > 0 {
		completionHeight = len(m.completions)
	}

	vpHeight := m.height - titleHeight - separatorHeight - vpBorderHeight - inputHeight - helpHeight - completionHeight
	if vpHeight < 1 {
		vpHeight = 1
	}

	// Viewport inner width (minus border padding)
	vpInnerWidth := m.width - 2

	if !m.ready {
		m.viewport = viewport.New(vpInnerWidth, vpHeight)
		m.viewport.SetContent(m.history.String())
		m.ready = true
	} else {
		m.viewport.Width = vpInnerWidth
		m.viewport.Height = vpHeight
	}

	m.textarea.SetWidth(m.width - 2) // match viewport inner width
}

func (m *chatModel) handleInput(input string) tea.Cmd {
	switch input {
	case "/quit", "/exit":
		return tea.Quit
	case "/new":
		if err := m.pool.Reset(defaultSessionId); err != nil {
			m.history.WriteString(errorStyle.Render("error: "+err.Error()) + "\n\n")
		} else {
			m.history.WriteString(systemStyle.Render("[new session started]") + "\n\n")
		}
		m.viewport.SetContent(m.history.String())
		m.viewport.GotoBottom()
		return nil
	case "/model":
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

	m.history.WriteString(userStyle.Render("You") + "\n" + input + "\n\n")
	m.history.WriteString(agentStyle.Render("Anna") + "\n")
	m.viewport.SetContent(m.history.String())
	m.viewport.GotoBottom()

	m.streaming = true
	m.status = "Thinking..."
	m.textarea.Blur()

	ctx := m.ctx
	return func() tea.Msg {
		stream := m.pool.Chat(ctx, defaultSessionId, input)
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

		if err := m.pool.Reset(defaultSessionId); err != nil {
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

	// Title bar: "Anna" left, "provider/model" right
	title := titleStyle.Render(" Anna")
	modelInfo := modelInfoStyle.Render(fmt.Sprintf("%s/%s ", m.provider, m.model))
	titleGap := m.width - lipgloss.Width(title) - lipgloss.Width(modelInfo)
	if titleGap < 0 {
		titleGap = 0
	}
	titleBar := title + strings.Repeat(" ", titleGap) + modelInfo

	// Separator
	separator := separatorStyle.Render(strings.Repeat("─", m.width))

	// Viewport with rounded border
	vpBorder := viewportBorder.Width(m.width - 2)
	chatPanel := vpBorder.Render(m.viewport.View())

	// Input area with matching border
	inputBorder := viewportBorder.Width(m.width - 2)
	input := inputBorder.Render(m.textarea.View())

	// Completion popup (between input and help bar)
	completionView := ""
	if m.completing && len(m.completions) > 0 {
		completionView = renderCompletions(m.completions, m.completeCursor)
	}

	// Help bar: commands left, status right
	helpText := " /new · /model · /quit · ctrl+c · pgup/pgdn scroll"
	if m.picking {
		helpText = " Type to filter · ↑/↓ navigate · Enter select · Esc cancel"
	} else if m.completing {
		helpText = " ↑/↓ navigate · Tab complete · Enter submit · Esc cancel"
	}
	help := helpStyle.Render(helpText)
	status := ""
	if m.status != "" {
		status = statusStyle.Render(m.status + " ")
	}
	helpGap := m.width - lipgloss.Width(help) - lipgloss.Width(status)
	if helpGap < 0 {
		helpGap = 0
	}
	helpBar := help + strings.Repeat(" ", helpGap) + status

	if completionView != "" {
		return fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s", titleBar, separator, chatPanel, input, completionView, helpBar)
	}
	return fmt.Sprintf("%s\n%s\n%s\n%s\n%s", titleBar, separator, chatPanel, input, helpBar)
}
