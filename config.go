package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/caarlos0/env/v11"
	"github.com/vaayne/anna/agent"
	"github.com/vaayne/anna/ai/types"
	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration for anna.
// Env vars use ANNA_ prefix (e.g. ANNA_PROVIDER, ANNA_MODEL).
type Config struct {
	Provider    string                    `yaml:"provider"     env:"PROVIDER"`
	Model       string                    `yaml:"model"        env:"MODEL"`
	ModelStrong string                    `yaml:"model_strong" env:"MODEL_STRONG"`
	ModelFast   string                    `yaml:"model_fast"   env:"MODEL_FAST"`
	Workspace   string                    `yaml:"workspace"    env:"WORKSPACE"`
	Runner      RunnerConfig              `yaml:"runner"       envPrefix:"RUNNER_"`
	Cron        CronConfig                `yaml:"cron"         envPrefix:"CRON_"`
	Providers   map[string]ProviderConfig `yaml:"providers"`
	Channels    ChannelsConfig            `yaml:"channels"`
}

// Model tier constants.
const (
	ModelTierStrong = "strong"
	ModelTierFast   = "fast"
)

// ChannelsConfig groups all channel (interface) configurations.
type ChannelsConfig struct {
	Telegram TelegramConfig `yaml:"telegram" envPrefix:"TELEGRAM_"`
}

type CronConfig struct {
	Enabled *bool  `yaml:"enabled"  env:"ENABLED"`
	DataDir string `yaml:"data_dir" env:"DATA_DIR"`
}

// CronEnabled returns whether cron is enabled (defaults to true).
func (c CronConfig) CronEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

type ProviderConfig struct {
	APIKey  string        `yaml:"api_key"`
	BaseURL string        `yaml:"base_url"`
	Models  []ModelConfig `yaml:"models"`
}

type ModelConfig struct {
	ID            string            `yaml:"id"`
	Name          string            `yaml:"name"`
	API           string            `yaml:"api"`
	Reasoning     bool              `yaml:"reasoning"`
	Input         []string          `yaml:"input"`
	ContextWindow int               `yaml:"context_window"`
	MaxTokens     int               `yaml:"max_tokens"`
	Headers       map[string]string `yaml:"headers"`
	Cost          *ModelCostConfig  `yaml:"cost"`
}

type ModelCostConfig struct {
	Input      float64 `yaml:"input"`
	Output     float64 `yaml:"output"`
	CacheRead  float64 `yaml:"cache_read"`
	CacheWrite float64 `yaml:"cache_write"`
}

type RunnerConfig struct {
	Type        string           `yaml:"type"         env:"TYPE"`
	System      string           `yaml:"system"       env:"SYSTEM"`
	IdleTimeout int              `yaml:"idle_timeout" env:"IDLE_TIMEOUT"`
	Compaction  CompactionConfig `yaml:"compaction"   envPrefix:"COMPACTION_"`
}

// CompactionConfig is an alias for agent.CompactionConfig for config YAML binding.
type CompactionConfig = agent.CompactionConfig

type TelegramConfig struct {
	Token      string  `yaml:"token"       env:"TOKEN"`
	NotifyChat string  `yaml:"notify_chat" env:"NOTIFY_CHAT"`
	ChannelID  string  `yaml:"channel_id"  env:"CHANNEL_ID"`
	GroupMode  string  `yaml:"group_mode"  env:"GROUP_MODE"`
	AllowedIDs []int64 `yaml:"allowed_ids" env:"ALLOWED_IDS"`
}

// annaHome returns the anna home directory.
// Priority: ANNA_HOME env → ~/.anna
func annaHome() string {
	if v := os.Getenv("ANNA_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".anna")
	}
	return filepath.Join(home, ".anna")
}

// configPath returns the path to config.yaml inside the anna home.
func configPath() string {
	return filepath.Join(annaHome(), "config.yaml")
}

// StatePath returns the path to state.yaml (mutable runtime state) inside the workspace.
func (cfg *Config) StatePath() string {
	return filepath.Join(cfg.Workspace, "state.yaml")
}

// cachePath returns the cache directory inside the anna home.
func cachePath() string {
	return filepath.Join(annaHome(), "cache")
}

// LoadConfig loads config from the default anna home (~/.anna/config.yaml).
func LoadConfig() (*Config, error) {
	return loadConfigFrom(annaHome())
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

	// Resolve workspace early so state.yaml can be found.
	// Priority: ANNA_WORKSPACE env → yaml → default.
	if cfg.Workspace == "" {
		if v := os.Getenv("ANNA_WORKSPACE"); v != "" {
			cfg.Workspace = v
		} else {
			cfg.Workspace = filepath.Join(dir, "workspace")
		}
	}

	// Apply runtime state overrides (state.yaml) — mutable values like
	// current provider/model set by "anna models set" or /model command.
	applyState(cfg)

	// Apply environment variable overrides (ANNA_ prefix).
	// Uses caarlos0/env struct tags; only set env vars override YAML values.
	if err := env.ParseWithOptions(cfg, env.Options{Prefix: "ANNA_"}); err != nil {
		return nil, fmt.Errorf("parse env vars: %w", err)
	}

	// Initialize providers map if nil.
	if cfg.Providers == nil {
		cfg.Providers = make(map[string]ProviderConfig)
	}

	// Resolve provider env vars for known providers.
	// These use standard env var names (ANTHROPIC_API_KEY, etc.) not ANNA_ prefix.
	resolveProviderEnv(cfg, "anthropic", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL")
	resolveProviderEnv(cfg, "openai", "OPENAI_API_KEY", "OPENAI_BASE_URL")
	resolveProviderEnv(cfg, "openai-response", "OPENAI_API_KEY", "OPENAI_BASE_URL")

	// Apply defaults for missing values.
	if cfg.Provider == "" {
		cfg.Provider = "anthropic"
	}
	if cfg.Model == "" {
		cfg.Model = "claude-sonnet-4-6"
	}
	if cfg.Runner.Type == "" {
		cfg.Runner.Type = "go"
	}
	if cfg.Runner.IdleTimeout == 0 {
		cfg.Runner.IdleTimeout = 10
	}
	if cfg.Cron.DataDir == "" {
		cfg.Cron.DataDir = filepath.Join(cfg.Workspace, "cron")
	}

	return cfg, nil
}

// resolveProviderEnv fills in api_key and base_url from environment variables
// if not already set in the config.
func resolveProviderEnv(cfg *Config, name, keyEnv, urlEnv string) {
	p := cfg.Providers[name]
	if p.APIKey == "" {
		if v := os.Getenv(keyEnv); v != "" {
			p.APIKey = v
		}
	}
	if p.BaseURL == "" {
		if v := os.Getenv(urlEnv); v != "" {
			p.BaseURL = v
		}
	}
	cfg.Providers[name] = p
}

// Workspace path helpers — all data lives under Workspace.

func (cfg *Config) SessionsPath() string {
	return filepath.Join(cfg.Workspace, "sessions")
}

func (cfg *Config) MemoryPath() string {
	return filepath.Join(cfg.Workspace, "memory")
}

func (cfg *Config) SkillsPath() string {
	return filepath.Join(cfg.Workspace, "skills")
}

func (cfg *Config) ModelsPath() string {
	return filepath.Join(cachePath(), "models.json")
}

func (cfg *Config) LogPath() string {
	return filepath.Join(cfg.Workspace, "anna.log")
}

// ResolveModel returns the types.Model for the default provider/model,
// looking up from the provider's model list config.
func (cfg *Config) ResolveModel() types.Model {
	return cfg.ResolveModelTier(ModelTierStrong)
}

// ResolveModelTier returns the model for the given tier after applying
// the fallback: strong → model, fast → model.
func (cfg *Config) ResolveModelTier(tier string) types.Model {
	modelID := cfg.resolveModelID(tier)
	providerCfg := cfg.Providers[cfg.Provider]
	for _, m := range providerCfg.Models {
		if m.ID == modelID {
			return modelConfigToType(cfg.Provider, m)
		}
	}
	// Fallback: construct a minimal Model from defaults.
	return types.Model{
		ID:       modelID,
		Name:     modelID,
		API:      cfg.Provider,
		Provider: cfg.Provider,
		BaseURL:  providerCfg.BaseURL,
	}
}

// resolveModelID returns the model ID string for the given tier,
// falling back to Model if the tier-specific value is not set.
func (cfg *Config) resolveModelID(tier string) string {
	switch tier {
	case ModelTierStrong:
		if cfg.ModelStrong != "" {
			return cfg.ModelStrong
		}
		return cfg.Model
	case ModelTierFast:
		if cfg.ModelFast != "" {
			return cfg.ModelFast
		}
		return cfg.Model
	default:
		if cfg.ModelStrong != "" {
			return cfg.ModelStrong
		}
		return cfg.Model
	}
}

// SaveModelSelection persists the provider and model to state.yaml
// in the given workspace, keeping config.yaml as a static, user-edited file.
func SaveModelSelection(workspace, provider, model string) error {
	path := filepath.Join(workspace, "state.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	raw := make(map[string]any)
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read state: %w", err)
	}
	if err == nil {
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parse state: %w", err)
		}
	}

	raw["provider"] = provider
	raw["model"] = model

	out, err := yaml.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	return nil
}

// applyState loads state.yaml from the workspace and overrides provider/model in cfg.
func applyState(cfg *Config) {
	path := cfg.StatePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("failed to read state file", "path", path, "error", err)
		}
		return
	}
	var state struct {
		Provider string `yaml:"provider"`
		Model    string `yaml:"model"`
	}
	if err := yaml.Unmarshal(data, &state); err != nil {
		return
	}
	if state.Provider != "" {
		cfg.Provider = state.Provider
	}
	if state.Model != "" {
		cfg.Model = state.Model
	}
}

func modelConfigToType(provider string, m ModelConfig) types.Model {
	model := types.Model{
		ID:            m.ID,
		Name:          m.ID,
		API:           m.API,
		Provider:      provider,
		Reasoning:     m.Reasoning,
		Input:         m.Input,
		ContextWindow: m.ContextWindow,
		MaxTokens:     m.MaxTokens,
		Headers:       m.Headers,
	}
	if model.Name == "" {
		model.Name = m.Name
	}
	if model.API == "" {
		model.API = provider
	}
	if m.Cost != nil {
		model.Cost = types.ModelCost{
			Input:      m.Cost.Input,
			Output:     m.Cost.Output,
			CacheRead:  m.Cost.CacheRead,
			CacheWrite: m.Cost.CacheWrite,
		}
	}
	return model
}
