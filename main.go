package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vaayne/pibot/agent"
	"github.com/vaayne/pibot/bot"
	"github.com/vaayne/pibot/cli"
)

const usage = `Usage: pibot <command> [flags]

Commands:
  chat      Start interactive CLI chat
  telegram  Start Telegram bot

Flags (chat):
  --stream  Read prompt from stdin and stream response to stdout`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Println(usage)
		return fmt.Errorf("no command specified")
	}

	cmd := args[0]

	if cmd == "--help" || cmd == "-h" {
		fmt.Println(usage)
		return nil
	}

	cfg, err := LoadConfig()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Create context that cancels on SIGINT/SIGTERM.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Create session manager.
	idleTimeout := time.Duration(cfg.Pi.IdleTimeout) * time.Minute
	sm := agent.NewSessionManager(cfg.Pi.Binary, cfg.Pi.Model, cfg.Sessions, idleTimeout)
	go sm.StartReaper(ctx)
	defer sm.StopAll()

	// Check for --stream flag in remaining args.
	stream := false
	for _, a := range args[1:] {
		if a == "--stream" {
			stream = true
		}
	}

	switch cmd {
	case "chat":
		if stream {
			return cli.RunStream(ctx, sm)
		}
		return cli.RunChat(ctx, sm)
	case "telegram":
		if cfg.Telegram.Token == "" {
			return fmt.Errorf("Telegram token not configured. Set in .agents/config.yaml or PIBOT_TELEGRAM_TOKEN env var")
		}
		log.Println("pibot: starting Telegram bot...")
		if err := bot.RunTelegram(ctx, cfg.Telegram.Token, sm); err != nil && ctx.Err() == nil {
			return fmt.Errorf("telegram: %w", err)
		}
		log.Println("pibot: Telegram bot stopped.")
		return nil
	default:
		fmt.Println(usage)
		return fmt.Errorf("unknown command: %s", cmd)
	}
}
