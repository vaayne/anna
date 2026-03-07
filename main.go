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
	"github.com/vaayne/anna/agent/runner/go/tool"
	"github.com/vaayne/anna/agent/store"
	"github.com/vaayne/anna/channel"
	clicmd "github.com/vaayne/anna/channel/cli"
	"github.com/vaayne/anna/channel/telegram"
	"github.com/vaayne/anna/cron"
	"github.com/vaayne/anna/memory"
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
			modelsCommand(),
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

			s, err := setup(c.Context)
			if err != nil {
				return err
			}
			defer s.pool.Close()

			if s.cronSvc != nil {
				if err := s.cronSvc.Start(s.ctx); err != nil {
					return fmt.Errorf("start cron: %w", err)
				}
				defer s.cronSvc.Stop()
			}

			if c.Bool("stream") {
				return clicmd.RunStream(s.ctx, s.pool)
			}
			listFn := func() []channel.ModelOption { return collectModels(s.cfg) }
			switchFn := modelSwitcher(s.cfg, s.pool, s.memStore, s.extraTools)
			return clicmd.RunChat(s.ctx, s.pool, s.cfg.Provider, s.cfg.Model, listFn, switchFn)
		},
	}
}

func gatewayCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "gateway",
		Usage: "Start daemon services (Telegram, etc.) based on config",
		Action: func(c *ucli.Context) error {
			s, err := setup(c.Context)
			if err != nil {
				return err
			}
			defer s.pool.Close()

			if s.cronSvc != nil {
				if err := s.cronSvc.Start(s.ctx); err != nil {
					return fmt.Errorf("start cron: %w", err)
				}
				defer s.cronSvc.Stop()
			}

			listFn := func() []channel.ModelOption { return collectModels(s.cfg) }
			switchFn := modelSwitcher(s.cfg, s.pool, s.memStore, s.extraTools)
			return runGateway(s.ctx, s.cfg, s.pool, listFn, switchFn)
		},
	}
}

type setupResult struct {
	ctx        context.Context
	cfg        *Config
	pool       *agent.Pool
	cronSvc    *cron.Service
	memStore   *memory.Store
	extraTools []tool.Tool
}

func setup(parent context.Context) (*setupResult, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	ctx, cancel := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	_ = cancel // cancel is deferred via the caller's lifecycle

	// Create cron service and tool before the runner factory so the tool
	// can be injected into the Go runner.
	var cronSvc *cron.Service
	var extraTools []tool.Tool
	if cfg.Cron.CronEnabled() {
		cronSvc, err = cron.New(cfg.Cron.DataDir)
		if err != nil {
			return nil, fmt.Errorf("create cron service: %w", err)
		}
		extraTools = append(extraTools, cron.NewTool(cronSvc))
	}

	// Memory store + tool — always available.
	memStore := memory.NewStore(filepath.Join(configDir(), "memory"))
	extraTools = append(extraTools, memory.NewTool(memStore))

	idleTimeout := time.Duration(cfg.Runner.IdleTimeout) * time.Minute
	factory, err := newRunnerFactory(cfg, memStore, extraTools)
	if err != nil {
		return nil, fmt.Errorf("create runner factory: %w", err)
	}

	opts := []agent.PoolOption{
		agent.WithIdleTimeout(idleTimeout),
		agent.WithCompaction(cfg.Runner.Compaction.WithDefaults()),
		agent.WithDefaultModel(cfg.resolveModelID(ModelTierStrong)),
		agent.WithFastModel(cfg.resolveModelID(ModelTierFast)),
	}
	if cfg.Sessions != "" {
		cwd, _ := os.Getwd()
		s, err := store.NewFileStore(cfg.Sessions, cwd)
		if err != nil {
			return nil, fmt.Errorf("create session store: %w", err)
		}
		opts = append(opts, agent.WithStore(s))
		slog.Info("session persistence enabled", "dir", cfg.Sessions)
	}

	pool := agent.NewPool(factory, opts...)
	go pool.StartReaper(ctx)

	// Wire the cron callback now that pool exists.
	if cronSvc != nil {
		cronSvc.SetOnJob(func(ctx context.Context, job cron.Job) {
			sessionID := "cron:" + job.ID
			msg := fmt.Sprintf("[Scheduled Task] %s\n\nInstruction: %s", job.Name, job.Message)
			ch := pool.Chat(ctx, sessionID, msg)
			for evt := range ch {
				if evt.Err != nil {
					slog.Error("cron job error", "job_id", job.ID, "error", evt.Err)
				}
			}
		})
	}

	return &setupResult{
		ctx:        ctx,
		cfg:        cfg,
		pool:       pool,
		cronSvc:    cronSvc,
		memStore:   memStore,
		extraTools: extraTools,
	}, nil
}

func newRunnerFactory(cfg *Config, memStore *memory.Store, extraTools []tool.Tool) (runner.NewRunnerFunc, error) {
	switch cfg.Runner.Type {
	case "go":
		providerCfg := cfg.Providers[cfg.Provider]
		return func(ctx context.Context, model string) (runner.Runner, error) {
			if model == "" {
				model = cfg.Model
			}
			return gorunner.New(ctx, gorunner.Config{
				API:         cfg.Provider,
				Model:       model,
				APIKey:      providerCfg.APIKey,
				AgentsDir:   configDir(),
				MemoryStore: memStore,
				BaseURL:     providerCfg.BaseURL,
				ExtraTools:  extraTools,
			})
		}, nil
	default:
		return nil, fmt.Errorf("unknown runner type: %q", cfg.Runner.Type)
	}
}

// modelSwitcher returns a function that switches the pool's runner factory
// to use a different provider/model combination.
func modelSwitcher(cfg *Config, pool *agent.Pool, memStore *memory.Store, extraTools []tool.Tool) channel.ModelSwitchFunc {
	return func(provider, model string) error {
		cfg.Provider = provider
		cfg.Model = model
		factory, err := newRunnerFactory(cfg, memStore, extraTools)
		if err != nil {
			return err
		}
		pool.SetFactory(factory)
		pool.SetDefaultModel(model)
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

func runGateway(ctx context.Context, cfg *Config, pool *agent.Pool, listFn channel.ModelListFunc, switchFn channel.ModelSwitchFunc) error {
	started := 0

	if cfg.Telegram.Token != "" {
		started++
		slog.Info("starting telegram bot")
		if err := telegram.Run(ctx, cfg.Telegram.Token, pool, listFn, switchFn); err != nil && ctx.Err() == nil {
			return fmt.Errorf("telegram: %w", err)
		}
	}

	if started == 0 {
		return fmt.Errorf("no gateway services configured. Check .agents/config.yaml")
	}

	slog.Info("gateway stopped")
	return nil
}
