package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Runner   RunnerConfig   `yaml:"runner"`
	Telegram TelegramConfig `yaml:"telegram"`
	Sessions string         `yaml:"sessions"`
}

type RunnerConfig struct {
	Type        string        `yaml:"type"`
	Process     ProcessConfig `yaml:"process"`
	Go          GoConfig      `yaml:"go"`
	IdleTimeout int           `yaml:"idle_timeout"`
}

type ProcessConfig struct {
	Binary string `yaml:"binary"`
	Model  string `yaml:"model"`
}

type GoConfig struct {
	API     string `yaml:"api"`
	Model   string `yaml:"model"`
	APIKey  string `yaml:"api_key"`
	System  string `yaml:"system"`
	BaseURL string `yaml:"base_url"`
}

type TelegramConfig struct {
	Token string `yaml:"token"`
}

func configDir() string {
	return filepath.Join(".", ".agents")
}

func configPath() string {
	return filepath.Join(configDir(), "config.yaml")
}

// LoadConfig loads config from the default path (.agents/config.yaml).
func LoadConfig() (*Config, error) {
	return loadConfigFrom(configDir())
}

// loadConfigFrom loads config from the given directory.
func loadConfigFrom(dir string) (*Config, error) {
	cfg := &Config{}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err == nil {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}

	// Apply environment variable overrides.
	if v := os.Getenv("ANNA_TELEGRAM_TOKEN"); v != "" {
		cfg.Telegram.Token = v
	}
	if v := os.Getenv("ANNA_PI_BINARY"); v != "" {
		cfg.Runner.Process.Binary = v
	}
	if v := os.Getenv("ANNA_PI_MODEL"); v != "" {
		cfg.Runner.Process.Model = v
	}
	if v := os.Getenv("ANNA_RUNNER_TYPE"); v != "" {
		cfg.Runner.Type = v
	}
	if v := os.Getenv("ANNA_GO_MODEL"); v != "" {
		cfg.Runner.Go.Model = v
	}

	// Go runner: resolve API key and base URL from standard provider env vars.
	if cfg.Runner.Go.API == "" {
		cfg.Runner.Go.API = "anthropic"
	}
	if cfg.Runner.Go.Model == "" {
		cfg.Runner.Go.Model = "claude-sonnet-4-20250514"
	}
	switch cfg.Runner.Go.API {
	case "anthropic":
		if cfg.Runner.Go.APIKey == "" {
			cfg.Runner.Go.APIKey = os.Getenv("ANTHROPIC_API_KEY")
		}
		if cfg.Runner.Go.BaseURL == "" {
			cfg.Runner.Go.BaseURL = os.Getenv("ANTHROPIC_BASE_URL")
		}
	case "openai":
		if cfg.Runner.Go.APIKey == "" {
			cfg.Runner.Go.APIKey = os.Getenv("OPENAI_API_KEY")
		}
		if cfg.Runner.Go.BaseURL == "" {
			cfg.Runner.Go.BaseURL = os.Getenv("OPENAI_BASE_URL")
		}
	}

	// Apply defaults for missing values.
	if cfg.Runner.Type == "" {
		cfg.Runner.Type = "process"
	}
	if cfg.Runner.Process.Binary == "" {
		cfg.Runner.Process.Binary = "pi"
	}
	if cfg.Runner.IdleTimeout == 0 {
		cfg.Runner.IdleTimeout = 10
	}
	if cfg.Sessions == "" {
		cfg.Sessions = filepath.Join(dir, "workspace", "sessions")
	}

	return cfg, nil
}
