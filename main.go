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

	"sort"

	ucli "github.com/urfave/cli/v2"
	"github.com/vaayne/anna/agent"
	"github.com/vaayne/anna/agent/runner"
	gorunner "github.com/vaayne/anna/agent/runner/go"
	"github.com/vaayne/anna/agent/runner/pi"
	"github.com/vaayne/anna/channel"
	clicmd "github.com/vaayne/anna/channel/cli"
	"github.com/vaayne/anna/channel/telegram"

	"github.com/vaayne/anna/pkg/ai/providers/anthropic"
	"github.com/vaayne/anna/pkg/ai/providers/openai"
	openairesponse "github.com/vaayne/anna/pkg/ai/providers/openai-response"
	"github.com/vaayne/anna/pkg/ai/stream"
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
			models := collectModels(cfg)
			switchFn := modelSwitcher(cfg, pool)
			return clicmd.RunChat(ctx, pool, cfg.Provider, cfg.Model, models, switchFn)
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

			models := collectModels(cfg)
			switchFn := modelSwitcher(cfg, pool)
			return runGateway(ctx, cfg, pool, models, switchFn)
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

// collectModels builds the list of available provider/model pairs from config.
// It includes the current active model, models from provider configs, and
// models fetched via ListModels API from each provider.
func collectModels(cfg *Config) []channel.ModelOption {
	seen := make(map[string]bool)
	var models []channel.ModelOption

	add := func(provider, model string) {
		key := provider + "/" + model
		if seen[key] {
			return
		}
		seen[key] = true
		models = append(models, channel.ModelOption{Provider: provider, Model: model})
	}

	// Current model first.
	add(cfg.Provider, cfg.Model)

	// Stable iteration order for providers.
	provNames := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		provNames = append(provNames, name)
	}
	sort.Strings(provNames)

	for _, provName := range provNames {
		prov := cfg.Providers[provName]

		// Auto-generate entry using the default model for each provider.
		add(provName, cfg.Model)

		// Explicitly listed models from config.
		for _, m := range prov.Models {
			add(provName, m.ID)
		}

		// Fetch models from the provider API.
		if provider := newStreamProvider(provName, prov); provider != nil {
			if lister, ok := provider.(stream.ModelLister); ok {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				listed, err := lister.ListModels(ctx)
				cancel()
				if err != nil {
					slog.Warn("failed to list models from provider", "provider", provName, "error", err)
					continue
				}
				for _, m := range listed {
					add(provName, m.ID)
				}
			}
		}
	}

	return models
}

// newStreamProvider creates a stream.Provider for the given provider name and config.
func newStreamProvider(name string, cfg ProviderConfig) stream.Provider {
	switch name {
	case "anthropic":
		return anthropic.New(anthropic.Config{BaseURL: cfg.BaseURL, APIKey: cfg.APIKey})
	case "openai":
		return openai.New(openai.Config{BaseURL: cfg.BaseURL, APIKey: cfg.APIKey})
	case "openai-response":
		return openairesponse.New(openairesponse.Config{BaseURL: cfg.BaseURL, APIKey: cfg.APIKey})
	default:
		return nil
	}
}

// modelSwitcher returns a function that switches the pool's runner factory
// to use a different provider/model combination.
func modelSwitcher(cfg *Config, pool *agent.Pool) channel.ModelSwitchFunc {
	return func(provider, model string) error {
		cfg.Provider = provider
		cfg.Model = model
		factory, err := newRunnerFactory(cfg)
		if err != nil {
			return err
		}
		pool.SetFactory(factory)
		if err := SaveModelSelection(provider, model); err != nil {
			slog.Warn("failed to persist model selection", "error", err)
		}
		return nil
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

func runGateway(ctx context.Context, cfg *Config, pool *agent.Pool, models []channel.ModelOption, switchFn channel.ModelSwitchFunc) error {
	started := 0

	if cfg.Telegram.Token != "" {
		started++
		slog.Info("starting telegram bot")
		if err := telegram.Run(ctx, cfg.Telegram.Token, pool, models, switchFn); err != nil && ctx.Err() == nil {
			return fmt.Errorf("telegram: %w", err)
		}
	}

	if started == 0 {
		return fmt.Errorf("no gateway services configured. Check .agents/config.yaml")
	}

	slog.Info("gateway stopped")
	return nil
}
