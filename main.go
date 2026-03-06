package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	ucli "github.com/urfave/cli/v2"
	"github.com/vaayne/anna/agent"
	"github.com/vaayne/anna/agent/runner"
	gorunner "github.com/vaayne/anna/agent/runner/go"
	"github.com/vaayne/anna/agent/runner/pi"
	clicmd "github.com/vaayne/anna/channel/cli"
	"github.com/vaayne/anna/channel/telegram"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))

	app := newApp()

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newApp() *ucli.App {
	return &ucli.App{
		Name:  "anna",
		Usage: "A local AI assistant",
		Commands: []*ucli.Command{
			chatCommand(),
			gatewayCommand(),
		},
	}
}

func chatCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "chat",
		Usage: "Start interactive CLI chat",
		Flags: []ucli.Flag{
			&ucli.BoolFlag{
				Name:  "stream",
				Usage: "Read prompt from stdin and stream response to stdout",
			},
		},
		Action: func(c *ucli.Context) error {
			if !c.Bool("stream") {
				if err := setupLogFile(); err != nil {
					return fmt.Errorf("setup log file: %w", err)
				}
			}

			ctx, cfg, pool, err := setup(c.Context)
			if err != nil {
				return err
			}
			defer pool.Close()

			if c.Bool("stream") {
				return clicmd.RunStream(ctx, pool)
			}
			return clicmd.RunChat(ctx, pool, cfg.Provider, cfg.Model)
		},
	}
}

func gatewayCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "gateway",
		Usage: "Start daemon services (Telegram, etc.) based on config",
		Action: func(c *ucli.Context) error {
			ctx, cfg, pool, err := setup(c.Context)
			if err != nil {
				return err
			}
			defer pool.Close()

			return runGateway(ctx, cfg, pool)
		},
	}
}

func setup(parent context.Context) (context.Context, *Config, *agent.Pool, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("loading config: %w", err)
	}

	ctx, cancel := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	_ = cancel // cancel is deferred via the caller's lifecycle

	idleTimeout := time.Duration(cfg.Runner.IdleTimeout) * time.Minute
	factory, err := newRunnerFactory(cfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create runner factory: %w", err)
	}
	pool := agent.NewPool(factory, agent.WithIdleTimeout(idleTimeout))
	go pool.StartReaper(ctx)

	return ctx, cfg, pool, nil
}

func newRunnerFactory(cfg *Config) (runner.NewRunnerFunc, error) {
	switch cfg.Runner.Type {
	case "process":
		return func(ctx context.Context) (runner.Runner, error) {
			return pi.New(ctx, cfg.Runner.Process.Binary, cfg.Runner.Process.Model)
		}, nil
	case "go":
		providerCfg := cfg.Providers[cfg.Provider]
		return func(ctx context.Context) (runner.Runner, error) {
			return gorunner.New(ctx, gorunner.Config{
				API:     cfg.Provider,
				Model:   cfg.Model,
				APIKey:  providerCfg.APIKey,
				System:  cfg.Runner.System,
				BaseURL: providerCfg.BaseURL,
			})
		}, nil
	default:
		return nil, fmt.Errorf("unknown runner type: %q", cfg.Runner.Type)
	}
}

func setupLogFile() error {
	logPath := filepath.Join(configDir(), "anna.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))
	return nil
}

func runGateway(ctx context.Context, cfg *Config, pool *agent.Pool) error {
	started := 0

	if cfg.Telegram.Token != "" {
		started++
		slog.Info("starting telegram bot")
		if err := telegram.Run(ctx, cfg.Telegram.Token, pool); err != nil && ctx.Err() == nil {
			return fmt.Errorf("telegram: %w", err)
		}
	}

	if started == 0 {
		return fmt.Errorf("no gateway services configured. Check .agents/config.yaml")
	}

	slog.Info("gateway stopped")
	return nil
}
