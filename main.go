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

const usage = `Usage: pibot <command>

Commands:
  chat      Start interactive CLI chat
  telegram  Start Telegram bot`

func main() {
	if len(os.Args) < 2 {
		fmt.Println(usage)
		os.Exit(1)
	}

	cmd := os.Args[1]

	if cmd == "--help" || cmd == "-h" {
		fmt.Println(usage)
		return
	}

	cfg, err := LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	// Create context that cancels on SIGINT/SIGTERM.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Create session manager.
	idleTimeout := time.Duration(cfg.Pi.IdleTimeout) * time.Minute
	sm := agent.NewSessionManager(cfg.Pi.Binary, cfg.Sessions, idleTimeout)
	go sm.StartReaper(ctx)
	defer sm.StopAll()

	switch cmd {
	case "chat":
		if err := cli.RunChat(ctx, sm); err != nil {
			log.Fatalf("chat: %v", err)
		}
	case "telegram":
		if cfg.Telegram.Token == "" {
			fmt.Fprintln(os.Stderr, "Error: Telegram token not configured. Set in ~/.pibot/config.yaml or PIBOT_TELEGRAM_TOKEN env var.")
			os.Exit(1)
		}
		log.Println("pibot: starting Telegram bot...")
		if err := bot.RunTelegram(ctx, cfg.Telegram.Token, sm); err != nil && ctx.Err() == nil {
			log.Fatalf("telegram: %v", err)
		}
		log.Println("pibot: Telegram bot stopped.")
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", cmd)
		fmt.Println(usage)
		os.Exit(1)
	}
}
