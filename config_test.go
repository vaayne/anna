package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	if cfg.Runner.Type != "process" {
		t.Errorf("Runner.Type = %q, want %q", cfg.Runner.Type, "process")
	}
	if cfg.Runner.Process.Binary != "pi" {
		t.Errorf("Runner.Process.Binary = %q, want %q", cfg.Runner.Process.Binary, "pi")
	}
	if cfg.Runner.IdleTimeout != 10 {
		t.Errorf("Runner.IdleTimeout = %d, want 10", cfg.Runner.IdleTimeout)
	}
	if cfg.Sessions != filepath.Join(dir, "workspace", "sessions") {
		t.Errorf("Sessions = %q, want %q", cfg.Sessions, filepath.Join(dir, "workspace", "sessions"))
	}
	if cfg.Telegram.Token != "" {
		t.Errorf("Telegram.Token = %q, want empty", cfg.Telegram.Token)
	}
	if cfg.Provider != "anthropic" {
		t.Errorf("Provider = %q, want %q", cfg.Provider, "anthropic")
	}
	if cfg.Model != "claude-sonnet-4-20250514" {
		t.Errorf("Model = %q, want %q", cfg.Model, "claude-sonnet-4-20250514")
	}
}

func TestLoadConfigFromFile(t *testing.T) {
	dir := t.TempDir()
	yaml := `
runner:
  type: process
  process:
    binary: "/usr/local/bin/pi"
  idle_timeout: 5
telegram:
  token: "test-token-123"
sessions: "/tmp/sessions"
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	if cfg.Runner.Process.Binary != "/usr/local/bin/pi" {
		t.Errorf("Runner.Process.Binary = %q, want %q", cfg.Runner.Process.Binary, "/usr/local/bin/pi")
	}
	if cfg.Runner.IdleTimeout != 5 {
		t.Errorf("Runner.IdleTimeout = %d, want 5", cfg.Runner.IdleTimeout)
	}
	if cfg.Telegram.Token != "test-token-123" {
		t.Errorf("Telegram.Token = %q, want %q", cfg.Telegram.Token, "test-token-123")
	}
	if cfg.Sessions != "/tmp/sessions" {
		t.Errorf("Sessions = %q, want %q", cfg.Sessions, "/tmp/sessions")
	}
}

func TestLoadConfigEnvOverrides(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("ANNA_TELEGRAM_TOKEN", "env-token")
	t.Setenv("ANNA_PI_BINARY", "/env/pi")

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	if cfg.Telegram.Token != "env-token" {
		t.Errorf("Telegram.Token = %q, want %q", cfg.Telegram.Token, "env-token")
	}
	if cfg.Runner.Process.Binary != "/env/pi" {
		t.Errorf("Runner.Process.Binary = %q, want %q", cfg.Runner.Process.Binary, "/env/pi")
	}
}

func TestLoadConfigEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	yaml := `
telegram:
  token: "file-token"
runner:
  process:
    binary: "file-pi"
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ANNA_TELEGRAM_TOKEN", "env-token")
	t.Setenv("ANNA_PI_BINARY", "")

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	// Env var overrides file value.
	if cfg.Telegram.Token != "env-token" {
		t.Errorf("Telegram.Token = %q, want %q", cfg.Telegram.Token, "env-token")
	}
	// Empty env var does NOT override.
	if cfg.Runner.Process.Binary != "file-pi" {
		t.Errorf("Runner.Process.Binary = %q, want %q", cfg.Runner.Process.Binary, "file-pi")
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

func TestConfigDir(t *testing.T) {
	dir := configDir()
	if !strings.HasSuffix(dir, ".agents") {
		t.Errorf("configDir() = %q, want suffix .agents", dir)
	}
}

func TestConfigPath(t *testing.T) {
	p := configPath()
	if !strings.HasSuffix(p, filepath.Join(".agents", "config.yaml")) {
		t.Errorf("configPath() = %q, want suffix .agents/config.yaml", p)
	}
}

func TestLoadConfig(t *testing.T) {
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Runner.Process.Binary == "" {
		t.Error("Runner.Process.Binary should have a default")
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
	if cfg.Model != "claude-sonnet-4-20250514" {
		t.Errorf("Model = %q, want default %q", cfg.Model, "claude-sonnet-4-20250514")
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
		Provider: "anthropic",
		Model:    "test-model",
		Providers: map[string]ProviderConfig{
			"anthropic": {APIKey: "test-key"},
		},
		Runner: RunnerConfig{Type: "go"},
	}

	factory, err := newRunnerFactory(cfg)
	if err != nil {
		t.Fatalf("newRunnerFactory: %v", err)
	}

	r, err := factory(context.Background())
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

	_, err := newRunnerFactory(cfg)
	if err == nil {
		t.Fatal("expected error for unknown runner type")
	}
	if !strings.Contains(err.Error(), "unknown runner type") {
		t.Errorf("error = %q, want contains 'unknown runner type'", err.Error())
	}
}

func TestRunGatewayNoServices(t *testing.T) {
	t.Setenv("ANNA_TELEGRAM_TOKEN", "")
	orig, _ := os.Getwd()
	os.Chdir(t.TempDir())
	defer os.Chdir(orig)
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
model: claude-sonnet-4-20250514
providers:
  anthropic:
    api_key: "yaml-key"
    base_url: "https://yaml-proxy.example.com"
    models:
      - id: claude-sonnet-4-20250514
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
	if ant.Models[0].ID != "claude-sonnet-4-20250514" {
		t.Errorf("model ID = %q, want %q", ant.Models[0].ID, "claude-sonnet-4-20250514")
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
		Model:    "claude-sonnet-4-20250514",
		Providers: map[string]ProviderConfig{
			"anthropic": {
				APIKey: "key",
				Models: []ModelConfig{
					{
						ID:            "claude-sonnet-4-20250514",
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
	if model.ID != "claude-sonnet-4-20250514" {
		t.Errorf("model.ID = %q, want %q", model.ID, "claude-sonnet-4-20250514")
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
		Model:     "claude-sonnet-4-20250514",
		Providers: map[string]ProviderConfig{"anthropic": {APIKey: "key"}},
	}

	model := cfg.ResolveModel()
	if model.ID != "claude-sonnet-4-20250514" {
		t.Errorf("model.ID = %q, want %q", model.ID, "claude-sonnet-4-20250514")
	}
	if model.API != "anthropic" {
		t.Errorf("model.API = %q, want %q (fallback to provider name)", model.API, "anthropic")
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
