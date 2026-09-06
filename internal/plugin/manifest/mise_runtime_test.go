package manifest

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	pkgsandbox "github.com/CherryHQ/stella/pkg/sandbox"
)

func TestRuntimeMiseEnv_PerUser(t *testing.T) {
	stellaHome := t.TempDir()
	userToolsDir := filepath.Join(stellaHome, "users", "u1", ".mise-tools")
	userConfigDir := filepath.Join(stellaHome, "users", "u1", "data", ".config", "mise")
	workspace := "/home/agent/workspace"

	env := RuntimeMiseEnv(stellaHome, userToolsDir, userConfigDir, workspace)

	if got := env["MISE_DATA_DIR"]; got != userToolsDir {
		t.Fatalf("MISE_DATA_DIR = %q, want per-user dir %q", got, userToolsDir)
	}
	if got, want := env["BASH_ENV"], pkgsandbox.ShellEnvPath(stellaHome); got != want {
		t.Fatalf("BASH_ENV = %q, want %q", got, want)
	}
	if got := env[pkgsandbox.EnvRunnerPath]; got != "" {
		t.Fatalf("%s = %q, want empty baseline", pkgsandbox.EnvRunnerPath, got)
	}
	for key, sub := range map[string]string{
		"MISE_CACHE_DIR": "cache",
		"MISE_STATE_DIR": "state",
	} {
		want := filepath.Join(userToolsDir, sub)
		if env[key] != want {
			t.Fatalf("%s = %q, want %q (under per-user tree)", key, env[key], want)
		}
	}
	if got := env["MISE_CONFIG_DIR"]; got != userConfigDir {
		t.Fatalf("MISE_CONFIG_DIR = %q, want principal config dir %q", got, userConfigDir)
	}
	if got, want := env["MISE_GLOBAL_CONFIG_FILE"], filepath.Join(userConfigDir, "config.toml"); got != want {
		t.Fatalf("MISE_GLOBAL_CONFIG_FILE = %q, want principal config %q", got, want)
	}
	if env["MISE_NOT_FOUND_AUTO_INSTALL"] != "true" {
		t.Fatalf("auto-install should be enabled for a writable per-user tree, got %q", env["MISE_NOT_FOUND_AUTO_INSTALL"])
	}

	// The system layer is supplied only by the immutable snapshot selection.
	if _, ok := env["MISE_SYSTEM_CONFIG_FILE"]; ok {
		t.Fatalf("MISE_SYSTEM_CONFIG_FILE must stay unset without a snapshot selection, got %q", env["MISE_SYSTEM_CONFIG_FILE"])
	}

	// Principal-global and project paths are trusted in precedence order.
	trusted := strings.Split(env["MISE_TRUSTED_CONFIG_PATHS"], string(filepath.ListSeparator))
	for _, want := range []string{userConfigDir, pkgsandbox.MountWorkspace, workspace} {
		if !slices.Contains(trusted, want) {
			t.Fatalf("trusted paths %v missing %q", trusted, want)
		}
	}
}

func TestRuntimeMiseEnv_NoUserHasNoSystemFallback(t *testing.T) {
	stellaHome := t.TempDir()

	env := RuntimeMiseEnv(stellaHome, "", "", "")

	wantData := filepath.Join(stellaHome, ".mise-tools")
	if env["MISE_DATA_DIR"] != wantData {
		t.Fatalf("MISE_DATA_DIR = %q, want system tree %q", env["MISE_DATA_DIR"], wantData)
	}
	if got, want := env["BASH_ENV"], pkgsandbox.ShellEnvPath(stellaHome); got != want {
		t.Fatalf("BASH_ENV = %q, want %q", got, want)
	}
	if env["MISE_NOT_FOUND_AUTO_INSTALL"] != "false" {
		t.Fatalf("auto-install must stay off without a writable tree, got %q", env["MISE_NOT_FOUND_AUTO_INSTALL"])
	}
	if _, ok := env["MISE_SYSTEM_CONFIG_FILE"]; ok {
		t.Fatalf("MISE_SYSTEM_CONFIG_FILE must stay unset without a snapshot selection, got %q", env["MISE_SYSTEM_CONFIG_FILE"])
	}
	for _, key := range []string{"MISE_CONFIG_DIR", "MISE_CACHE_DIR", "MISE_STATE_DIR", "MISE_GLOBAL_CONFIG_FILE"} {
		if value, ok := env[key]; ok {
			t.Fatalf("%s should follow sandbox XDG roots without a writable tree, got %q", key, value)
		}
	}
}
