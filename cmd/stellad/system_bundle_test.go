package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ucli "github.com/urfave/cli/v2"

	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/plugins/core"
	"github.com/CherryHQ/stella/resources/binaries"
)

var errSystemBundleWriter = errors.New("system bundle output failed")

type failingSystemBundleWriter struct{}

func (failingSystemBundleWriter) Write([]byte) (int, error) { return 0, errSystemBundleWriter }

func systemBundleTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := binaries.EnsureTools(home); err != nil {
		t.Fatalf("ensure embedded test runtimes: %v", err)
	}
	identity, err := core.RuntimeIdentity()
	if err != nil {
		t.Fatalf("runtime identity: %v", err)
	}
	publicDir := filepath.Join(home, ".mise-tools", "public", identity)
	if err := os.MkdirAll(publicDir, 0o755); err != nil {
		t.Fatalf("create prepared core selection: %v", err)
	}
	if err := os.Symlink(filepath.Join(home, "bin", ".stella-shell-env"), filepath.Join(publicDir, ".stella-shell-env")); err != nil {
		t.Fatalf("prepare shell environment fixture: %v", err)
	}
	for _, resource := range core.RuntimeResources() {
		path := filepath.Join(publicDir, resource.Name)
		source := filepath.Join(home, "bin", resource.Name)
		if _, err := os.Stat(source); err != nil {
			if !os.IsNotExist(err) {
				t.Fatalf("inspect runtime %q: %v", resource.Name, err)
			}
			if err := os.WriteFile(source, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatalf("write runtime fixture %q: %v", resource.Name, err)
			}
		}
		if err := os.Symlink(source, path); err != nil {
			t.Fatalf("prepare runtime fixture %q: %v", resource.Name, err)
		}
	}
	return home
}

func TestSystemBundleCommandsUseConfiguredTemporaryHome(t *testing.T) {
	home := systemBundleTestHome(t)
	t.Setenv("STELLA_HOME", home)
	config.ResetStellaHome()
	t.Cleanup(config.ResetStellaHome)

	for _, args := range [][]string{
		{"stellad", "system-bundle", "revision"},
		{"stellad", "system-bundle", "install"},
		{"stellad", "system-bundle", "verify"},
	} {
		app := newApp()
		var output bytes.Buffer
		app.Writer = &output
		if err := app.RunContext(context.Background(), args); err != nil {
			t.Fatalf("%s: %v", strings.Join(args[1:], " "), err)
		}
		if strings.TrimSpace(output.String()) == "" {
			t.Fatalf("%s produced no stdout", strings.Join(args[1:], " "))
		}
	}
}

func TestSystemBundleCommandsPropagateWriterErrors(t *testing.T) {
	home := systemBundleTestHome(t)
	t.Setenv("STELLA_HOME", home)
	config.ResetStellaHome()
	t.Cleanup(config.ResetStellaHome)

	for _, args := range [][]string{
		{"stellad", "system-bundle", "revision"},
		{"stellad", "system-bundle", "install"},
		{"stellad", "system-bundle", "verify"},
	} {
		app := newApp()
		app.Writer = failingSystemBundleWriter{}
		err := app.RunContext(context.Background(), args)
		if !errors.Is(err, errSystemBundleWriter) {
			t.Fatalf("%s error = %v, want writer error", strings.Join(args[1:], " "), err)
		}
	}
}

func TestSystemBundleInstallExposesBuiltinArtifactPreparationFlag(t *testing.T) {
	command := systemBundleInstallCommand()
	flag := command.Flags[1].(*ucli.BoolFlag)
	if flag.Name != "prepare-builtin-artifacts" {
		t.Fatalf("artifact flag name = %q, want prepare-builtin-artifacts", flag.Name)
	}
}

func TestPublishCoreRuntimePathUsesExactPlan(t *testing.T) {
	publicDir := t.TempDir()
	target := filepath.Join(t.TempDir(), "core-runtime")
	plan := core.RuntimePlan{PublicDir: publicDir}
	if err := publishCoreRuntimePath(plan, target); err != nil {
		t.Fatalf("publishCoreRuntimePath: %v", err)
	}
	got, err := os.Readlink(target)
	if err != nil {
		t.Fatalf("read published core path: %v", err)
	}
	if filepath.Clean(got) != filepath.Clean(publicDir) {
		t.Fatalf("published core path = %q, want %q", got, publicDir)
	}
	if err := publishCoreRuntimePath(plan, target); err != nil {
		t.Fatalf("republishCoreRuntimePath: %v", err)
	}
	if err := publishCoreRuntimePath(plan, "relative"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative core path error = %v, want absolute path validation", err)
	}
}
