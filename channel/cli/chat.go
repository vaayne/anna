package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/vaayne/anna/agent"
	"github.com/vaayne/anna/agent/runner"
)

// streamStartMsg carries the stream channel from the agent.
type streamStartMsg struct {
	stream <-chan runner.Event
}

// streamChunkMsg carries a text delta from the agent stream.
type streamChunkMsg string

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

	provider  string
	model     string
	history   *strings.Builder
	streaming bool
	status    string
	width     int
	height    int
	ready     bool
}

func newChatModel(ctx context.Context, pool *agent.Pool, provider, model string) chatModel {
	ta := textarea.New()
	ta.Placeholder = "Type a message... (Enter to send, Alt+Enter for newline)"
	ta.Focus()
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.SetHeight(3)

	return chatModel{
		ctx:      ctx,
		pool:     pool,
		textarea: ta,
		provider: provider,
		model:    model,
		history:  &strings.Builder{},
	}
}

func (m chatModel) Init() tea.Cmd {
	return textarea.Blink
}

func (m chatModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			return m, tea.Quit
		case tea.KeyEnter:
			if m.streaming {
				break
			}
			input := strings.TrimSpace(m.textarea.Value())
			if input == "" {
				break
			}
			m.textarea.Reset()
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
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

func (m *chatModel) resize() {
	// Layout height budget:
	// - Title bar: 1 line
	// - Separator: 1 line
	// - Viewport border: 2 lines (top + bottom)
	// - Input (textarea height + 2 for border)
	// - Help bar: 1 line
	titleHeight := 1
	separatorHeight := 1
	vpBorderHeight := 2
	inputHeight := m.textarea.Height() + 2 // textarea + border
	helpHeight := 1

	vpHeight := m.height - titleHeight - separatorHeight - vpBorderHeight - inputHeight - helpHeight
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
		return streamChunkMsg(evt.Text)
	}
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

	// Help bar: commands left, status right
	help := helpStyle.Render(" /new · /quit · ctrl+c")
	status := ""
	if m.status != "" {
		status = statusStyle.Render(m.status + " ")
	}
	helpGap := m.width - lipgloss.Width(help) - lipgloss.Width(status)
	if helpGap < 0 {
		helpGap = 0
	}
	helpBar := help + strings.Repeat(" ", helpGap) + status

	return fmt.Sprintf("%s\n%s\n%s\n%s\n%s", titleBar, separator, chatPanel, input, helpBar)
}
