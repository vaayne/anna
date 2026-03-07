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

	if cfg.Runner.Type != "go" {
		t.Errorf("Runner.Type = %q, want %q", cfg.Runner.Type, "go")
	}
	if cfg.Runner.IdleTimeout != 10 {
		t.Errorf("Runner.IdleTimeout = %d, want 10", cfg.Runner.IdleTimeout)
	}
	if cfg.SessionsPath() != filepath.Join(dir, "workspace", "sessions") {
		t.Errorf("SessionsPath() = %q, want %q", cfg.SessionsPath(), filepath.Join(dir, "workspace", "sessions"))
	}
	if cfg.Channels.Telegram.Token != "" {
		t.Errorf("Channels.Telegram.Token = %q, want empty", cfg.Channels.Telegram.Token)
	}
	if cfg.Provider != "anthropic" {
		t.Errorf("Provider = %q, want %q", cfg.Provider, "anthropic")
	}
	if cfg.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want %q", cfg.Model, "claude-sonnet-4-6")
	}
	if cfg.Workspace != filepath.Join(dir, "workspace") {
		t.Errorf("Workspace = %q, want %q", cfg.Workspace, filepath.Join(dir, "workspace"))
	}
}

func TestLoadConfigFromFile(t *testing.T) {
	dir := t.TempDir()
	yaml := `
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

	if cfg.Runner.IdleTimeout != 5 {
		t.Errorf("Runner.IdleTimeout = %d, want 5", cfg.Runner.IdleTimeout)
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
	if cfg.Runner.Type == "" {
		t.Error("Runner.Type should have a default")
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

	if cfg.Provider != "openai" {
		t.Errorf("Provider = %q, want %q", cfg.Provider, "openai")
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

	if cfg.Provider != "anthropic" {
		t.Errorf("Provider = %q, want default %q", cfg.Provider, "anthropic")
	}
	if cfg.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want default %q", cfg.Model, "claude-sonnet-4-6")
	}
}

func TestRunnerTypeEnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ANNA_RUNNER_TYPE", "go")

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	if cfg.Runner.Type != "go" {
		t.Errorf("Runner.Type = %q, want %q", cfg.Runner.Type, "go")
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

	if cfg.Provider != "openai" {
		t.Errorf("Provider = %q, want %q", cfg.Provider, "openai")
	}
	if cfg.Model != "gpt-4o" {
		t.Errorf("Model = %q, want %q", cfg.Model, "gpt-4o")
	}
}

func TestNewRunnerFactoryGo(t *testing.T) {
	cfg := &Config{
		Provider:  "anthropic",
		Model:     "test-model",
		Workspace: t.TempDir(),
		Runner:    RunnerConfig{Type: "go"},
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
		Runner: RunnerConfig{Type: "invalid"},
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
		Provider: "anthropic",
		Model:    "claude-sonnet-4-6",
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
		Provider:  "anthropic",
		Model:     "claude-sonnet-4-6",
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
			name:   "strong falls back to model",
			cfg:    Config{Provider: "anthropic", Model: "default-model", Providers: map[string]ProviderConfig{"anthropic": {}}},
			tier:   "strong",
			wantID: "default-model",
		},
		{
			name:   "strong uses model_strong",
			cfg:    Config{Provider: "anthropic", Model: "default-model", ModelStrong: "strong-model", Providers: map[string]ProviderConfig{"anthropic": {}}},
			tier:   "strong",
			wantID: "strong-model",
		},
		{
			name:   "fast falls back to model",
			cfg:    Config{Provider: "anthropic", Model: "default-model", Providers: map[string]ProviderConfig{"anthropic": {}}},
			tier:   "fast",
			wantID: "default-model",
		},
		{
			name:   "fast uses model_fast",
			cfg:    Config{Provider: "anthropic", Model: "default-model", ModelStrong: "s", ModelFast: "fast-model", Providers: map[string]ProviderConfig{"anthropic": {}}},
			tier:   "fast",
			wantID: "fast-model",
		},
		{
			name:   "unknown tier falls back like strong",
			cfg:    Config{Provider: "anthropic", Model: "default-model", ModelStrong: "strong-model", Providers: map[string]ProviderConfig{"anthropic": {}}},
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

func TestModelTiersFromYAML(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `
provider: anthropic
model: claude-sonnet-4-6
model_strong: claude-opus-4-6
model_fast: claude-haiku-4-5
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	if cfg.ModelStrong != "claude-opus-4-6" {
		t.Errorf("ModelStrong = %q, want %q", cfg.ModelStrong, "claude-opus-4-6")
	}
	if cfg.ModelFast != "claude-haiku-4-5" {
		t.Errorf("ModelFast = %q, want %q", cfg.ModelFast, "claude-haiku-4-5")
	}
}

func TestModelTierEnvOverrides(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("ANNA_MODEL_STRONG", "env-strong")
	t.Setenv("ANNA_MODEL_FAST", "env-fast")

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	if cfg.ModelStrong != "env-strong" {
		t.Errorf("ModelStrong = %q, want %q", cfg.ModelStrong, "env-strong")
	}
	if cfg.ModelFast != "env-fast" {
		t.Errorf("ModelFast = %q, want %q", cfg.ModelFast, "env-fast")
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
model_strong: yaml-strong
model_fast: yaml-fast
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
	if cfg.ModelStrong != "env-strong" {
		t.Errorf("ModelStrong = %q, want %q", cfg.ModelStrong, "env-strong")
	}
	// YAML value should remain for non-overridden tiers.
	if cfg.ModelFast != "yaml-fast" {
		t.Errorf("ModelFast = %q, want %q", cfg.ModelFast, "yaml-fast")
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

func TestStateOverridesConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ANNA_HOME", dir)

	configYAML := `
provider: anthropic
model: claude-sonnet-4-6
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// state.yaml lives under workspace (dir/workspace/state.yaml).
	wsDir := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stateYAML := `
provider: openai
model: gpt-4o
`
	if err := os.WriteFile(filepath.Join(wsDir, "state.yaml"), []byte(stateYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	if cfg.Provider != "openai" {
		t.Errorf("Provider = %q, want %q (state should override config)", cfg.Provider, "openai")
	}
	if cfg.Model != "gpt-4o" {
		t.Errorf("Model = %q, want %q (state should override config)", cfg.Model, "gpt-4o")
	}
}

func TestEnvOverridesState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ANNA_HOME", dir)
	t.Setenv("ANNA_PROVIDER", "anthropic")
	t.Setenv("ANNA_MODEL", "claude-opus-4-6")

	// state.yaml lives under workspace (dir/workspace/state.yaml).
	wsDir := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stateYAML := `
provider: openai
model: gpt-4o
`
	if err := os.WriteFile(filepath.Join(wsDir, "state.yaml"), []byte(stateYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	if cfg.Provider != "anthropic" {
		t.Errorf("Provider = %q, want %q (env should override state)", cfg.Provider, "anthropic")
	}
	if cfg.Model != "claude-opus-4-6" {
		t.Errorf("Model = %q, want %q (env should override state)", cfg.Model, "claude-opus-4-6")
	}
}

func TestSaveModelSelectionWritesState(t *testing.T) {
	wsDir := t.TempDir()

	if err := SaveModelSelection(wsDir, "openai", "gpt-4o"); err != nil {
		t.Fatalf("SaveModelSelection: %v", err)
	}

	// state.yaml should exist in workspace.
	data, err := os.ReadFile(filepath.Join(wsDir, "state.yaml"))
	if err != nil {
		t.Fatalf("read state.yaml: %v", err)
	}
	if !strings.Contains(string(data), "openai") {
		t.Errorf("state.yaml should contain provider, got: %s", data)
	}
}

func TestWorkspacePaths(t *testing.T) {
	cfg := &Config{
		Workspace: "/home/user/.anna/workspace",
	}

	if cfg.SessionsPath() != "/home/user/.anna/workspace/sessions" {
		t.Errorf("SessionsPath() = %q", cfg.SessionsPath())
	}
	if cfg.MemoryPath() != "/home/user/.anna/workspace/memory" {
		t.Errorf("MemoryPath() = %q", cfg.MemoryPath())
	}
	if cfg.SkillsPath() != "/home/user/.anna/workspace/skills" {
		t.Errorf("SkillsPath() = %q", cfg.SkillsPath())
	}
	wantModels := filepath.Join(cachePath(), "models.json")
	if cfg.ModelsPath() != wantModels {
		t.Errorf("ModelsPath() = %q, want %q", cfg.ModelsPath(), wantModels)
	}
	if cfg.LogPath() != "/home/user/.anna/workspace/anna.log" {
		t.Errorf("LogPath() = %q", cfg.LogPath())
	}
}
