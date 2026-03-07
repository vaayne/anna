package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("ANNA_RUNNER_TYPE", "")
	dir := t.TempDir()
	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	if cfg.Agents.Runner.Type != "go" {
		t.Errorf("Agents.Runner.Type = %q, want %q", cfg.Agents.Runner.Type, "go")
	}
	if cfg.Agents.Runner.IdleTimeout != 10 {
		t.Errorf("Agents.Runner.IdleTimeout = %d, want 10", cfg.Agents.Runner.IdleTimeout)
	}
	if cfg.SessionsPath() != filepath.Join(dir, "sessions") {
		t.Errorf("SessionsPath() = %q, want %q", cfg.SessionsPath(), filepath.Join(dir, "sessions"))
	}
	if cfg.Channels.Telegram.Token != "" {
		t.Errorf("Channels.Telegram.Token = %q, want empty", cfg.Channels.Telegram.Token)
	}
	if cfg.Agents.Provider != "anthropic" {
		t.Errorf("Agents.Provider = %q, want %q", cfg.Agents.Provider, "anthropic")
	}
	if cfg.Agents.Model != "claude-sonnet-4-6" {
		t.Errorf("Agents.Model = %q, want %q", cfg.Agents.Model, "claude-sonnet-4-6")
	}
	if cfg.Agents.Workspace != dir {
		t.Errorf("Agents.Workspace = %q, want %q", cfg.Agents.Workspace, dir)
	}
}

func TestLoadConfigFromFile(t *testing.T) {
	dir := t.TempDir()
	yaml := `
agents:
  runner:
    type: go
    idle_timeout: 5
channels:
  telegram:
    token: "test-token-123"
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	if cfg.Agents.Runner.IdleTimeout != 5 {
		t.Errorf("Agents.Runner.IdleTimeout = %d, want 5", cfg.Agents.Runner.IdleTimeout)
	}
	if cfg.Channels.Telegram.Token != "test-token-123" {
		t.Errorf("Channels.Telegram.Token = %q, want %q", cfg.Channels.Telegram.Token, "test-token-123")
	}
}

func TestLoadConfigEnvOverrides(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("ANNA_TELEGRAM_TOKEN", "env-token")

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	if cfg.Channels.Telegram.Token != "env-token" {
		t.Errorf("Channels.Telegram.Token = %q, want %q", cfg.Channels.Telegram.Token, "env-token")
	}
}

func TestLoadConfigEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	yaml := `
channels:
  telegram:
    token: "file-token"
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ANNA_TELEGRAM_TOKEN", "env-token")

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	// Env var overrides file value.
	if cfg.Channels.Telegram.Token != "env-token" {
		t.Errorf("Channels.Telegram.Token = %q, want %q", cfg.Channels.Telegram.Token, "env-token")
	}
}

func TestLoadConfigInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(":::invalid"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := loadConfigFrom(dir)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestLoadConfigCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")

	_, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("expected directory")
	}
}

func TestAnnaHome(t *testing.T) {
	t.Setenv("ANNA_HOME", "")
	dir := annaHome()
	if !strings.HasSuffix(dir, ".anna") {
		t.Errorf("annaHome() = %q, want suffix .anna", dir)
	}
}

func TestAnnaHomeEnv(t *testing.T) {
	t.Setenv("ANNA_HOME", "/custom/anna")
	dir := annaHome()
	if dir != "/custom/anna" {
		t.Errorf("annaHome() = %q, want %q", dir, "/custom/anna")
	}
}

func TestConfigPath(t *testing.T) {
	t.Setenv("ANNA_HOME", "")
	p := configPath()
	if !strings.HasSuffix(p, filepath.Join(".anna", "config.yaml")) {
		t.Errorf("configPath() = %q, want suffix .anna/config.yaml", p)
	}
}

func TestLoadConfig(t *testing.T) {
	t.Setenv("ANNA_HOME", t.TempDir())
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Agents.Runner.Type == "" {
		t.Error("Agents.Runner.Type should have a default")
	}
}

func TestRunHelp(t *testing.T) {
	app := newApp()
	err := app.Run([]string{"anna", "--help"})
	if err != nil {
		t.Fatalf("run --help: %v", err)
	}
}

func TestRunHelpShort(t *testing.T) {
	app := newApp()
	err := app.Run([]string{"anna", "-h"})
	if err != nil {
		t.Fatalf("run -h: %v", err)
	}
}

func TestProviderEnvAnthropicAPIKey(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-123")
	t.Setenv("ANTHROPIC_BASE_URL", "https://custom-proxy.example.com")
	t.Setenv("ANNA_RUNNER_TYPE", "go")

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	p := cfg.Providers["anthropic"]
	if p.APIKey != "sk-ant-test-123" {
		t.Errorf("Providers[anthropic].APIKey = %q, want %q", p.APIKey, "sk-ant-test-123")
	}
	if p.BaseURL != "https://custom-proxy.example.com" {
		t.Errorf("Providers[anthropic].BaseURL = %q, want %q", p.BaseURL, "https://custom-proxy.example.com")
	}
}

func TestProviderEnvOpenAI(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `
agents:
  provider: openai
  model: gpt-4o
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("OPENAI_API_KEY", "sk-openai-test")
	t.Setenv("OPENAI_BASE_URL", "https://openai-proxy.example.com")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	if cfg.Agents.Provider != "openai" {
		t.Errorf("Agents.Provider = %q, want %q", cfg.Agents.Provider, "openai")
	}
	p := cfg.Providers["openai"]
	if p.APIKey != "sk-openai-test" {
		t.Errorf("Providers[openai].APIKey = %q, want %q", p.APIKey, "sk-openai-test")
	}
	if p.BaseURL != "https://openai-proxy.example.com" {
		t.Errorf("Providers[openai].BaseURL = %q, want %q", p.BaseURL, "https://openai-proxy.example.com")
	}
}

func TestProviderDefaultValues(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ANNA_RUNNER_TYPE", "")
	t.Setenv("ANNA_PROVIDER", "")
	t.Setenv("ANNA_MODEL", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	if cfg.Agents.Provider != "anthropic" {
		t.Errorf("Agents.Provider = %q, want default %q", cfg.Agents.Provider, "anthropic")
	}
	if cfg.Agents.Model != "claude-sonnet-4-6" {
		t.Errorf("Agents.Model = %q, want default %q", cfg.Agents.Model, "claude-sonnet-4-6")
	}
}

func TestRunnerTypeEnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ANNA_RUNNER_TYPE", "go")

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	if cfg.Agents.Runner.Type != "go" {
		t.Errorf("Agents.Runner.Type = %q, want %q", cfg.Agents.Runner.Type, "go")
	}
}

func TestProviderModelEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ANNA_PROVIDER", "openai")
	t.Setenv("ANNA_MODEL", "gpt-4o")

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	if cfg.Agents.Provider != "openai" {
		t.Errorf("Agents.Provider = %q, want %q", cfg.Agents.Provider, "openai")
	}
	if cfg.Agents.Model != "gpt-4o" {
		t.Errorf("Agents.Model = %q, want %q", cfg.Agents.Model, "gpt-4o")
	}
}

func TestNewRunnerFactoryGo(t *testing.T) {
	cfg := &Config{
		Agents: AgentsConfig{
			Provider:  "anthropic",
			Model:     "test-model",
			Workspace: t.TempDir(),
			Runner:    RunnerConfig{Type: "go"},
		},
		Providers: map[string]ProviderConfig{
			"anthropic": {APIKey: "test-key"},
		},
	}

	factory, err := newRunnerFactory(cfg, nil, nil)
	if err != nil {
		t.Fatalf("newRunnerFactory: %v", err)
	}

	r, err := factory(context.Background(), "")
	if err != nil {
		t.Fatalf("factory: %v", err)
	}

	if r == nil {
		t.Fatal("expected non-nil runner")
	}
}

func TestNewRunnerFactoryUnknown(t *testing.T) {
	cfg := &Config{
		Agents: AgentsConfig{
			Runner: RunnerConfig{Type: "invalid"},
		},
	}

	_, err := newRunnerFactory(cfg, nil, nil)
	if err == nil {
		t.Fatal("expected error for unknown runner type")
	}
	if !strings.Contains(err.Error(), "unknown runner type") {
		t.Errorf("error = %q, want contains 'unknown runner type'", err.Error())
	}
}

func TestRunGatewayNoServices(t *testing.T) {
	t.Setenv("ANNA_TELEGRAM_TOKEN", "")
	t.Setenv("ANNA_HOME", t.TempDir())
	app := newApp()
	err := app.Run([]string{"anna", "gateway"})
	if err == nil {
		t.Fatal("expected error for no configured services")
	}
	if !strings.Contains(err.Error(), "no gateway services configured") {
		t.Errorf("err = %q, want contains 'no gateway services configured'", err.Error())
	}
}

func TestProvidersFromYAML(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `
agents:
  provider: anthropic
  model: claude-sonnet-4-6
providers:
  anthropic:
    api_key: "yaml-key"
    base_url: "https://yaml-proxy.example.com"
    models:
      - id: claude-sonnet-4-6
        name: Claude Sonnet 4
        api: anthropic-messages
        reasoning: false
        context_window: 200000
        max_tokens: 8192
  openai:
    api_key: "openai-yaml-key"
    models:
      - id: gpt-4o
        name: GPT-4o
        api: openai-completions
        context_window: 128000
        max_tokens: 4096
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Clear env to ensure YAML values are used.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	ant := cfg.Providers["anthropic"]
	if ant.APIKey != "yaml-key" {
		t.Errorf("Providers[anthropic].APIKey = %q, want %q", ant.APIKey, "yaml-key")
	}
	if ant.BaseURL != "https://yaml-proxy.example.com" {
		t.Errorf("Providers[anthropic].BaseURL = %q, want %q", ant.BaseURL, "https://yaml-proxy.example.com")
	}
	if len(ant.Models) != 1 {
		t.Fatalf("Providers[anthropic].Models len = %d, want 1", len(ant.Models))
	}
	if ant.Models[0].ID != "claude-sonnet-4-6" {
		t.Errorf("model ID = %q, want %q", ant.Models[0].ID, "claude-sonnet-4-6")
	}
	if ant.Models[0].ContextWindow != 200000 {
		t.Errorf("model ContextWindow = %d, want 200000", ant.Models[0].ContextWindow)
	}

	oai := cfg.Providers["openai"]
	if oai.APIKey != "openai-yaml-key" {
		t.Errorf("Providers[openai].APIKey = %q, want %q", oai.APIKey, "openai-yaml-key")
	}
	if len(oai.Models) != 1 {
		t.Fatalf("Providers[openai].Models len = %d, want 1", len(oai.Models))
	}
	if oai.Models[0].ID != "gpt-4o" {
		t.Errorf("model ID = %q, want %q", oai.Models[0].ID, "gpt-4o")
	}
}

func TestResolveModelFromConfig(t *testing.T) {
	cfg := &Config{
		Agents: AgentsConfig{
			Provider: "anthropic",
			Model:    "claude-sonnet-4-6",
		},
		Providers: map[string]ProviderConfig{
			"anthropic": {
				APIKey: "key",
				Models: []ModelConfig{
					{
						ID:            "claude-sonnet-4-6",
						Name:          "Claude Sonnet 4",
						API:           "anthropic-messages",
						ContextWindow: 200000,
						MaxTokens:     8192,
					},
				},
			},
		},
	}

	model := cfg.ResolveModel()
	if model.ID != "claude-sonnet-4-6" {
		t.Errorf("model.ID = %q, want %q", model.ID, "claude-sonnet-4-6")
	}
	if model.API != "anthropic-messages" {
		t.Errorf("model.API = %q, want %q", model.API, "anthropic-messages")
	}
	if model.ContextWindow != 200000 {
		t.Errorf("model.ContextWindow = %d, want 200000", model.ContextWindow)
	}
}

func TestResolveModelFallback(t *testing.T) {
	cfg := &Config{
		Agents: AgentsConfig{
			Provider: "anthropic",
			Model:    "claude-sonnet-4-6",
		},
		Providers: map[string]ProviderConfig{"anthropic": {APIKey: "key"}},
	}

	model := cfg.ResolveModel()
	if model.ID != "claude-sonnet-4-6" {
		t.Errorf("model.ID = %q, want %q", model.ID, "claude-sonnet-4-6")
	}
	if model.API != "anthropic" {
		t.Errorf("model.API = %q, want %q (fallback to provider name)", model.API, "anthropic")
	}
}

func TestResolveModelTierFallbackChain(t *testing.T) {
	tests := []struct {
		name   string
		cfg    Config
		tier   string
		wantID string
	}{
		{
			name:   "strong falls back to Model",
			cfg:    Config{Agents: AgentsConfig{Provider: "anthropic", Model: "default-model"}, Providers: map[string]ProviderConfig{"anthropic": {}}},
			tier:   "strong",
			wantID: "default-model",
		},
		{
			name:   "strong uses Models.Strong",
			cfg:    Config{Agents: AgentsConfig{Provider: "anthropic", Model: "default-model", Models: ModelsConfig{Strong: "strong-model"}}, Providers: map[string]ProviderConfig{"anthropic": {}}},
			tier:   "strong",
			wantID: "strong-model",
		},
		{
			name:   "worker falls back to strong",
			cfg:    Config{Agents: AgentsConfig{Provider: "anthropic", Model: "default-model", Models: ModelsConfig{Strong: "strong-model"}}, Providers: map[string]ProviderConfig{"anthropic": {}}},
			tier:   "worker",
			wantID: "strong-model",
		},
		{
			name:   "worker uses Models.Worker",
			cfg:    Config{Agents: AgentsConfig{Provider: "anthropic", Model: "default-model", Models: ModelsConfig{Strong: "strong-model", Worker: "worker-model"}}, Providers: map[string]ProviderConfig{"anthropic": {}}},
			tier:   "worker",
			wantID: "worker-model",
		},
		{
			name:   "fast falls back to worker",
			cfg:    Config{Agents: AgentsConfig{Provider: "anthropic", Model: "default-model", Models: ModelsConfig{Worker: "worker-model"}}, Providers: map[string]ProviderConfig{"anthropic": {}}},
			tier:   "fast",
			wantID: "worker-model",
		},
		{
			name:   "fast falls back to strong when no worker",
			cfg:    Config{Agents: AgentsConfig{Provider: "anthropic", Model: "default-model", Models: ModelsConfig{Strong: "strong-model"}}, Providers: map[string]ProviderConfig{"anthropic": {}}},
			tier:   "fast",
			wantID: "strong-model",
		},
		{
			name:   "fast falls back to Model when nothing set",
			cfg:    Config{Agents: AgentsConfig{Provider: "anthropic", Model: "default-model"}, Providers: map[string]ProviderConfig{"anthropic": {}}},
			tier:   "fast",
			wantID: "default-model",
		},
		{
			name:   "fast uses Models.Fast",
			cfg:    Config{Agents: AgentsConfig{Provider: "anthropic", Model: "default-model", Models: ModelsConfig{Strong: "s", Worker: "w", Fast: "fast-model"}}, Providers: map[string]ProviderConfig{"anthropic": {}}},
			tier:   "fast",
			wantID: "fast-model",
		},
		{
			name:   "unknown tier falls back like strong",
			cfg:    Config{Agents: AgentsConfig{Provider: "anthropic", Model: "default-model", Models: ModelsConfig{Strong: "strong-model"}}, Providers: map[string]ProviderConfig{"anthropic": {}}},
			tier:   "unknown",
			wantID: "strong-model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := tt.cfg.ResolveModelTier(tt.tier)
			if model.ID != tt.wantID {
				t.Errorf("ResolveModelTier(%q) = %q, want %q", tt.tier, model.ID, tt.wantID)
			}
		})
	}
}

func TestModelsConfigFromYAML(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `
agents:
  provider: anthropic
  model: claude-sonnet-4-6
  models:
    strong: claude-sonnet-4-6
    worker: claude-haiku-3.5
    fast: claude-haiku-3.5
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	if cfg.Agents.Models.Strong != "claude-sonnet-4-6" {
		t.Errorf("Agents.Models.Strong = %q, want %q", cfg.Agents.Models.Strong, "claude-sonnet-4-6")
	}
	if cfg.Agents.Models.Worker != "claude-haiku-3.5" {
		t.Errorf("Agents.Models.Worker = %q, want %q", cfg.Agents.Models.Worker, "claude-haiku-3.5")
	}
	if cfg.Agents.Models.Fast != "claude-haiku-3.5" {
		t.Errorf("Agents.Models.Fast = %q, want %q", cfg.Agents.Models.Fast, "claude-haiku-3.5")
	}
}

func TestModelTierEnvOverrides(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("ANNA_MODEL_STRONG", "env-strong")
	t.Setenv("ANNA_MODEL_WORKER", "env-worker")
	t.Setenv("ANNA_MODEL_FAST", "env-fast")

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	if cfg.Agents.Models.Strong != "env-strong" {
		t.Errorf("Agents.Models.Strong = %q, want %q", cfg.Agents.Models.Strong, "env-strong")
	}
	if cfg.Agents.Models.Worker != "env-worker" {
		t.Errorf("Agents.Models.Worker = %q, want %q", cfg.Agents.Models.Worker, "env-worker")
	}
	if cfg.Agents.Models.Fast != "env-fast" {
		t.Errorf("Agents.Models.Fast = %q, want %q", cfg.Agents.Models.Fast, "env-fast")
	}

	// Verify tier resolution uses env values.
	model := cfg.ResolveModelTier("fast")
	if model.ID != "env-fast" {
		t.Errorf("ResolveModelTier(fast) = %q, want %q", model.ID, "env-fast")
	}
}

func TestModelTierEnvOverridesYAML(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `
agents:
  models:
    strong: yaml-strong
    fast: yaml-fast
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ANNA_MODEL_STRONG", "env-strong")

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	// Env should override YAML.
	if cfg.Agents.Models.Strong != "env-strong" {
		t.Errorf("Agents.Models.Strong = %q, want %q", cfg.Agents.Models.Strong, "env-strong")
	}
	// YAML value should remain for non-overridden tiers.
	if cfg.Agents.Models.Fast != "yaml-fast" {
		t.Errorf("Agents.Models.Fast = %q, want %q", cfg.Agents.Models.Fast, "yaml-fast")
	}
}

func TestProviderEnvDoesNotOverrideYAML(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `
providers:
  anthropic:
    api_key: "yaml-key"
    base_url: "https://yaml.example.com"
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ANTHROPIC_API_KEY", "env-key")
	t.Setenv("ANTHROPIC_BASE_URL", "https://env.example.com")

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	p := cfg.Providers["anthropic"]
	// YAML values should NOT be overridden by env.
	if p.APIKey != "yaml-key" {
		t.Errorf("Providers[anthropic].APIKey = %q, want %q (YAML should win)", p.APIKey, "yaml-key")
	}
	if p.BaseURL != "https://yaml.example.com" {
		t.Errorf("Providers[anthropic].BaseURL = %q, want %q (YAML should win)", p.BaseURL, "https://yaml.example.com")
	}
}

func TestWorkspacePaths(t *testing.T) {
	cfg := &Config{
		Agents: AgentsConfig{
			Workspace: "/home/user/.anna",
		},
	}

	if cfg.SessionsPath() != "/home/user/.anna/sessions" {
		t.Errorf("SessionsPath() = %q", cfg.SessionsPath())
	}
	if cfg.MemoryPath() != "/home/user/.anna/memory" {
		t.Errorf("MemoryPath() = %q", cfg.MemoryPath())
	}
	if cfg.SkillsPath() != "/home/user/.anna/skills" {
		t.Errorf("SkillsPath() = %q", cfg.SkillsPath())
	}
	if cfg.ModelsPath() != "/home/user/.anna/models.json" {
		t.Errorf("ModelsPath() = %q", cfg.ModelsPath())
	}
	if cfg.LogPath() != "/home/user/.anna/anna.log" {
		t.Errorf("LogPath() = %q", cfg.LogPath())
	}
}
