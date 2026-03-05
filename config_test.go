package main

import (
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

	if cfg.Pi.Binary != "pi" {
		t.Errorf("Pi.Binary = %q, want %q", cfg.Pi.Binary, "pi")
	}
	if cfg.Pi.IdleTimeout != 10 {
		t.Errorf("Pi.IdleTimeout = %d, want 10", cfg.Pi.IdleTimeout)
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
pi:
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

	if cfg.Pi.Binary != "/usr/local/bin/pi" {
		t.Errorf("Pi.Binary = %q, want %q", cfg.Pi.Binary, "/usr/local/bin/pi")
	}
	if cfg.Pi.IdleTimeout != 5 {
		t.Errorf("Pi.IdleTimeout = %d, want 5", cfg.Pi.IdleTimeout)
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
	if cfg.Pi.Binary != "/env/pi" {
		t.Errorf("Pi.Binary = %q, want %q", cfg.Pi.Binary, "/env/pi")
	}
}

func TestLoadConfigEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	yaml := `
telegram:
  token: "file-token"
pi:
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
	if cfg.Pi.Binary != "file-pi" {
		t.Errorf("Pi.Binary = %q, want %q", cfg.Pi.Binary, "file-pi")
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
	// LoadConfig uses the real .agents/ dir. Just verify it doesn't error.
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Pi.Binary == "" {
		t.Error("Pi.Binary should have a default")
	}
}

func TestRunNoArgs(t *testing.T) {
	err := run(nil)
	if err == nil {
		t.Fatal("expected error for no args")
	}
}

func TestRunHelp(t *testing.T) {
	err := run([]string{"--help"})
	if err != nil {
		t.Fatalf("run --help: %v", err)
	}
}

func TestRunHelpShort(t *testing.T) {
	err := run([]string{"-h"})
	if err != nil {
		t.Fatalf("run -h: %v", err)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	err := run([]string{"foobar"})
	if err == nil {
		t.Fatal("expected error for unknown command")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("err = %q, want contains 'unknown command'", err.Error())
	}
}

func TestRunGatewayNoServices(t *testing.T) {
	t.Setenv("ANNA_TELEGRAM_TOKEN", "")
	orig, _ := os.Getwd()
	os.Chdir(t.TempDir())
	defer os.Chdir(orig)
	err := run([]string{"gateway"})
	if err == nil {
		t.Fatal("expected error for no configured services")
	}
	if !strings.Contains(err.Error(), "no gateway services configured") {
		t.Errorf("err = %q, want contains 'no gateway services configured'", err.Error())
	}
}
