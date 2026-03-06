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

func TestGoConfigAnthropicEnvVarResolution(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-123")
	t.Setenv("ANTHROPIC_BASE_URL", "https://custom-proxy.example.com")
	// Ensure runner type doesn't interfere.
	t.Setenv("ANNA_RUNNER_TYPE", "go")

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	if cfg.Runner.Go.APIKey != "sk-ant-test-123" {
		t.Errorf("Go.APIKey = %q, want %q", cfg.Runner.Go.APIKey, "sk-ant-test-123")
	}
	if cfg.Runner.Go.BaseURL != "https://custom-proxy.example.com" {
		t.Errorf("Go.BaseURL = %q, want %q", cfg.Runner.Go.BaseURL, "https://custom-proxy.example.com")
	}
}

func TestGoConfigOpenAIEnvVars(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `
runner:
  go:
    api: openai
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("OPENAI_API_KEY", "sk-openai-test")
	t.Setenv("OPENAI_BASE_URL", "https://openai-proxy.example.com")
	// Clear anthropic vars to avoid interference.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	if cfg.Runner.Go.API != "openai" {
		t.Errorf("Go.API = %q, want %q", cfg.Runner.Go.API, "openai")
	}
	if cfg.Runner.Go.APIKey != "sk-openai-test" {
		t.Errorf("Go.APIKey = %q, want %q", cfg.Runner.Go.APIKey, "sk-openai-test")
	}
	if cfg.Runner.Go.BaseURL != "https://openai-proxy.example.com" {
		t.Errorf("Go.BaseURL = %q, want %q", cfg.Runner.Go.BaseURL, "https://openai-proxy.example.com")
	}
}

func TestGoConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	// Clear env vars that could interfere.
	t.Setenv("ANNA_RUNNER_TYPE", "")
	t.Setenv("ANNA_GO_MODEL", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")

	cfg, err := loadConfigFrom(dir)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}

	if cfg.Runner.Go.API != "anthropic" {
		t.Errorf("Go.API = %q, want default %q", cfg.Runner.Go.API, "anthropic")
	}
	if cfg.Runner.Go.Model != "claude-sonnet-4-20250514" {
		t.Errorf("Go.Model = %q, want default %q", cfg.Runner.Go.Model, "claude-sonnet-4-20250514")
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

func TestNewRunnerFactoryGo(t *testing.T) {
	cfg := &Config{
		Runner: RunnerConfig{
			Type: "go",
			Go: GoConfig{
				API:    "anthropic",
				Model:  "test-model",
				APIKey: "test-key",
			},
		},
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
