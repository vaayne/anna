package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/vaayne/anna/agent"
	"github.com/vaayne/anna/channel"
)

// ModelOption re-exports channel.ModelOption for use by callers.
type ModelOption = channel.ModelOption

// ModelSwitchFunc re-exports channel.ModelSwitchFunc for use by callers.
type ModelSwitchFunc = channel.ModelSwitchFunc

const defaultSessionId = "session"

// RunStream reads all of stdin as a prompt, sends it to the agent, and streams the response to stdout.
func RunStream(ctx context.Context, pool *agent.Pool) error {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}

	prompt := strings.TrimSpace(string(input))
	if prompt == "" {
		return fmt.Errorf("empty prompt")
	}

	stream := pool.Chat(ctx, defaultSessionId, prompt)
	for evt := range stream {
		if evt.Err != nil {
			return evt.Err
		}
		fmt.Print(evt.Text)
	}
	fmt.Println()
	return nil
}

// RunChat starts an interactive terminal chat session using Bubble Tea.
func RunChat(ctx context.Context, pool *agent.Pool, provider, model string, models []ModelOption, switchFn ModelSwitchFunc) error {
	opts := make([]modelOption, len(models))
	for i, m := range models {
		opts[i] = modelOption{provider: m.Provider, model: m.Model}
	}
	m := newChatModel(ctx, pool, provider, model, opts, switchFn)
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}
	return nil
}
