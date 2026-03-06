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

// ModelListFunc re-exports channel.ModelListFunc for use by callers.
type ModelListFunc = channel.ModelListFunc

// ModelSwitchFunc re-exports channel.ModelSwitchFunc for use by callers.
type ModelSwitchFunc = channel.ModelSwitchFunc

const defaultStreamSessionId = "stream"

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

	stream := pool.Chat(ctx, defaultStreamSessionId, prompt)
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
func RunChat(ctx context.Context, pool *agent.Pool, provider, model string, listFn ModelListFunc, switchFn ModelSwitchFunc) error {
	m := newChatModel(ctx, pool, provider, model, listFn, switchFn)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}
	return nil
}
