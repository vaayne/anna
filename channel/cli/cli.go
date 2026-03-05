package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/vaayne/pibot/agent"
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

// RunChat starts an interactive terminal chat session using the given SessionProvider.
func RunChat(ctx context.Context, sm agent.SessionProvider) error {
	fmt.Println("pibot — type your message, /new for new session, /quit to exit")

	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Print(">>> ")

		if !scanner.Scan() {
			// EOF or scanner error
			break
		}

		line := strings.TrimSpace(scanner.Text())

		if line == "/quit" || line == "/exit" {
			return nil
		}

		if line == "/new" {
			if err := sm.NewSession(defaultSessionId); err != nil {
				fmt.Printf("error: %v\n", err)
			} else {
				fmt.Println("[new session started]")
			}
			continue
		}

		if line == "" {
			continue
		}

		ag, err := sm.GetOrCreate(ctx, defaultSessionId)
		if err != nil {
			fmt.Printf("error: failed to get agent: %v\n", err)
			continue
		}

		if !ag.Alive() {
			fmt.Println("[note: agent was restarted]")
		}

		stream := ag.SendPrompt(ctx, line)
		for evt := range stream {
			if evt.Err != nil {
				fmt.Printf("\nerror: %v\n", evt.Err)
				break
			}
			fmt.Print(evt.Text)
		}
		fmt.Println()
		fmt.Println()
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}

	return nil
}
