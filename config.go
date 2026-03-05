package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Pi       PiConfig       `yaml:"pi"`
	Telegram TelegramConfig `yaml:"telegram"`
	Sessions string         `yaml:"sessions"`
}

type PiConfig struct {
	Binary      string `yaml:"binary"`
	Model       string `yaml:"model"`
	IdleTimeout int    `yaml:"idle_timeout"`
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
	if v := os.Getenv("PIBOT_TELEGRAM_TOKEN"); v != "" {
		cfg.Telegram.Token = v
	}
	if v := os.Getenv("PIBOT_PI_BINARY"); v != "" {
		cfg.Pi.Binary = v
	}
	if v := os.Getenv("PIBOT_PI_MODEL"); v != "" {
		cfg.Pi.Model = v
	}

	// Apply defaults for missing values.
	if cfg.Pi.Binary == "" {
		cfg.Pi.Binary = "pi"
	}
	if cfg.Pi.IdleTimeout == 0 {
		cfg.Pi.IdleTimeout = 10
	}
	if cfg.Sessions == "" {
		cfg.Sessions = filepath.Join(dir, "workspace", "sessions")
	}

	return cfg, nil
}
