package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/vaayne/anna/agent"
)

// streamStartMsg carries the stream channel from the agent.
type streamStartMsg struct {
	stream <-chan agent.StreamEvent
}

// streamChunkMsg carries a text delta from the agent stream.
type streamChunkMsg string

// streamDoneMsg signals the stream has finished.
type streamDoneMsg struct{}

// streamErrMsg carries a streaming error.
type streamErrMsg struct{ err error }

type chatModel struct {
	ctx      context.Context
	sp       agent.SessionProvider
	textarea textarea.Model
	viewport viewport.Model
	stream   <-chan agent.StreamEvent

	history   *strings.Builder
	streaming bool
	width     int
	height    int
	ready     bool
}

func newChatModel(ctx context.Context, sp agent.SessionProvider) chatModel {
	ta := textarea.New()
	ta.Placeholder = "Type a message... (Enter to send, Alt+Enter for newline)"
	ta.Focus()
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.SetHeight(3)

	return chatModel{
		ctx:      ctx,
		sp:       sp,
		textarea: ta,
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
		m.history.WriteString(string(msg))
		m.viewport.SetContent(m.history.String())
		m.viewport.GotoBottom()
		return m, waitNextChunk(m.stream)

	case streamDoneMsg:
		m.streaming = false
		m.stream = nil
		m.history.WriteString("\n\n")
		m.viewport.SetContent(m.history.String())
		m.viewport.GotoBottom()
		m.textarea.Focus()
		return m, nil

	case streamErrMsg:
		m.streaming = false
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
	inputHeight := m.textarea.Height() + 2
	helpHeight := 1
	vpHeight := m.height - inputHeight - helpHeight - 1
	if vpHeight < 1 {
		vpHeight = 1
	}

	if !m.ready {
		m.viewport = viewport.New(m.width, vpHeight)
		m.viewport.SetContent(m.history.String())
		m.ready = true
	} else {
		m.viewport.Width = m.width
		m.viewport.Height = vpHeight
	}

	m.textarea.SetWidth(m.width)
}

func (m *chatModel) handleInput(input string) tea.Cmd {
	switch input {
	case "/quit", "/exit":
		return tea.Quit
	case "/new":
		if err := m.sp.NewSession(defaultSessionId); err != nil {
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
	m.textarea.Blur()

	ctx := m.ctx
	sp := m.sp
	return func() tea.Msg {
		ag, err := sp.GetOrCreate(ctx, defaultSessionId)
		if err != nil {
			return streamErrMsg{fmt.Errorf("failed to get agent: %w", err)}
		}
		if !ag.Alive() {
			return streamErrMsg{fmt.Errorf("agent was restarted, please try again")}
		}
		stream := ag.SendPrompt(ctx, input)
		return streamStartMsg{stream: stream}
	}
}

// waitNextChunk returns a Cmd that reads the next event from the stream channel.
func waitNextChunk(stream <-chan agent.StreamEvent) tea.Cmd {
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

	help := helpStyle.Render(" /new: new session • /quit: exit • ctrl+c: quit")
	return fmt.Sprintf("%s\n%s\n%s", m.viewport.View(), m.textarea.View(), help)
}
