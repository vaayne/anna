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
	IdleTimeout int    `yaml:"idle_timeout"`
}

type TelegramConfig struct {
	Token string `yaml:"token"`
}

func configDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".pibot")
}

func configPath() string {
	return filepath.Join(configDir(), "config.yaml")
}

func LoadConfig() (*Config, error) {
	cfg := &Config{}

	// Ensure config directory exists.
	dir := configDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}

	// Read config file if it exists.
	data, err := os.ReadFile(configPath())
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

	// Apply defaults for missing values.
	if cfg.Pi.Binary == "" {
		cfg.Pi.Binary = "pi"
	}
	if cfg.Pi.IdleTimeout == 0 {
		cfg.Pi.IdleTimeout = 10
	}
	if cfg.Sessions == "" {
		cfg.Sessions = filepath.Join(dir, "sessions")
	}

	return cfg, nil
}
