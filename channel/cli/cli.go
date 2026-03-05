package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/vaayne/anna/agent"
)

const defaultSessionId = "session"

// RunStream reads all of stdin as a prompt, sends it to the agent, and streams the response to stdout.
func RunStream(ctx context.Context, sm agent.SessionProvider) error {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}

	prompt := strings.TrimSpace(string(input))
	if prompt == "" {
		return fmt.Errorf("empty prompt")
	}

	ag, err := sm.GetOrCreate(ctx, defaultSessionId)
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}

	stream := ag.SendPrompt(ctx, prompt)
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
func RunChat(ctx context.Context, sm agent.SessionProvider) error {
	m := newChatModel(ctx, sm)
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}
	return nil
}
