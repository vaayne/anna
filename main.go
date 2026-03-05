package main

import (
	"fmt"
	"os"
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
	_ = cfg

	switch cmd {
	case "chat":
		fmt.Println("chat: not implemented")
	case "telegram":
		fmt.Println("telegram: not implemented")
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", cmd)
		fmt.Println(usage)
		os.Exit(1)
	}
}
