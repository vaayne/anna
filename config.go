package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/vaayne/anna/agent"
	"github.com/vaayne/anna/pkg/ai/types"
	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration for anna.
type Config struct {
	Provider    string                    `yaml:"provider"`
	Model       string                    `yaml:"model"`
	ModelStrong string                    `yaml:"model_strong"`
	ModelFast   string                    `yaml:"model_fast"`
	Workspace   string                    `yaml:"workspace"`
	Runner      RunnerConfig              `yaml:"runner"`
	Cron        CronConfig                `yaml:"cron"`
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
	Telegram TelegramConfig `yaml:"telegram"`
}

type CronConfig struct {
	Enabled *bool  `yaml:"enabled"`
	DataDir string `yaml:"data_dir"`
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
	Type        string           `yaml:"type"`
	System      string           `yaml:"system"`
	IdleTimeout int              `yaml:"idle_timeout"`
	Compaction  CompactionConfig `yaml:"compaction"`
}

// CompactionConfig is an alias for agent.CompactionConfig for config YAML binding.
type CompactionConfig = agent.CompactionConfig

type TelegramConfig struct {
	Token      string  `yaml:"token"`
	NotifyChat string  `yaml:"notify_chat"` // chat ID for proactive notifications
	ChannelID  string  `yaml:"channel_id"`  // broadcast channel (@name or numeric ID)
	GroupMode  string  `yaml:"group_mode"`  // "mention" | "always" | "disabled"
	AllowedIDs []int64 `yaml:"allowed_ids"` // user IDs allowed to use the bot (empty = allow all)
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

	// Apply environment variable overrides.
	if v := os.Getenv("ANNA_TELEGRAM_TOKEN"); v != "" {
		cfg.Channels.Telegram.Token = v
	}
	if v := os.Getenv("ANNA_TELEGRAM_NOTIFY_CHAT"); v != "" {
		cfg.Channels.Telegram.NotifyChat = v
	}
	if v := os.Getenv("ANNA_TELEGRAM_CHANNEL_ID"); v != "" {
		cfg.Channels.Telegram.ChannelID = v
	}
	if v := os.Getenv("ANNA_TELEGRAM_GROUP_MODE"); v != "" {
		cfg.Channels.Telegram.GroupMode = v
	}
	if v := os.Getenv("ANNA_RUNNER_TYPE"); v != "" {
		cfg.Runner.Type = v
	}
	if v := os.Getenv("ANNA_PROVIDER"); v != "" {
		cfg.Provider = v
	}
	if v := os.Getenv("ANNA_MODEL"); v != "" {
		cfg.Model = v
	}
	if v := os.Getenv("ANNA_MODEL_STRONG"); v != "" {
		cfg.ModelStrong = v
	}
	if v := os.Getenv("ANNA_MODEL_FAST"); v != "" {
		cfg.ModelFast = v
	}

	// Initialize providers map if nil.
	if cfg.Providers == nil {
		cfg.Providers = make(map[string]ProviderConfig)
	}

	// Resolve provider env vars for known providers.
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
	if cfg.Workspace == "" {
		cfg.Workspace = filepath.Join(dir, "workspace")
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
	return filepath.Join(cfg.Workspace, "models.json")
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

// SaveModelSelection persists the provider and model to the config file,
// preserving all other fields.
func SaveModelSelection(provider, model string) error {
	path := configPath()

	raw := make(map[string]any)
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read config: %w", err)
	}
	if err == nil {
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parse config: %w", err)
		}
	}

	raw["provider"] = provider
	raw["model"] = model

	out, err := yaml.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
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
