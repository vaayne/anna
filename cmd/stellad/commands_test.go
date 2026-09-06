package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ucli "github.com/urfave/cli/v2"

	"github.com/CherryHQ/stella/internal/db/dbtest"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/platform/home"
	"github.com/CherryHQ/stella/internal/skill"
	"github.com/CherryHQ/stella/pkg/ai"
	"github.com/CherryHQ/stella/pkg/providers"
	"github.com/CherryHQ/stella/resources/binaries"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

type commandTestProvider struct{}

func (commandTestProvider) API() string { return "anthropic" }
func (commandTestProvider) Stream(context.Context, ai.Model, ai.Context, ai.StreamOptions) (providers.AssistantEventStream, error) {
	return nil, errors.New("not implemented")
}

func TestIntentClassifierStreamFuncBuilderUsesProvidedProviderType(t *testing.T) {
	registry, err := providers.NewRegistry(providers.Definition{
		ID:   "openai",
		Name: "OpenAI",
		Build: func(config providers.Config) (providers.ProviderAdapter, error) {
			if got := config.APIKey; got != "k" {
				t.Fatalf("api_key = %#v, want %q", got, "k")
			}
			if got := config.BaseURL; got != "https://example.com" {
				t.Fatalf("base_url = %#v, want %q", got, "https://example.com")
			}
			return commandTestProvider{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	stream, err := intentClassifierStreamFuncBuilder(registry)(context.Background(), "openai", config.ProviderCreds{Type: "primary", APIKey: "k", BaseURL: "https://example.com"})
	if err != nil {
		t.Fatalf("intentClassifierStreamFuncBuilder: %v", err)
	}
	if stream == nil {
		t.Fatal("expected non-nil stream func")
	}
}

func setupCommandTestStellaHome(t *testing.T) string {
	t.Helper()
	stellaHome := t.TempDir()
	binDir := filepath.Join(stellaHome, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	_ = binaries.EnsureTools(stellaHome)
	t.Setenv("STELLA_HOME", stellaHome)
	config.ResetStellaHome()
	t.Cleanup(config.ResetStellaHome)
	return stellaHome
}

func TestEnsureEmbeddedAssetsBlocksLegacySkillWithoutMutation(t *testing.T) {
	stellaHome := setupCommandTestStellaHome(t)
	retiredBinary := filepath.Join(stellaHome, "bin", "stella")
	if err := os.WriteFile(retiredBinary, []byte("retired binary"), 0o755); err != nil {
		t.Fatalf("write retired binary: %v", err)
	}
	retired := filepath.Join(stellaHome, ".agents", "skills", "system", "kreuzberg")
	if err := os.MkdirAll(retired, 0o755); err != nil {
		t.Fatalf("create retired skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(retired, "SKILL.md"), []byte("stale"), 0o644); err != nil {
		t.Fatalf("write retired skill: %v", err)
	}

	if err := ensureEmbeddedAssets(); err == nil {
		t.Fatal("ensureEmbeddedAssets accepted legacy custom skill")
	} else {
		for _, instruction := range []string{
			"system/kreuzberg",
			"back up the listed paths",
			"previous working Stella binary",
			"Settings → Skills",
			"Admin Console → Deployment resources → Global Skills",
			"verify each import",
			"remove only migrated or residual legacy paths",
			"then retry",
		} {
			if !strings.Contains(err.Error(), instruction) {
				t.Errorf("ensureEmbeddedAssets() error = %q, want instruction %q", err, instruction)
			}
		}
	}
	if content, err := os.ReadFile(filepath.Join(retired, "SKILL.md")); err != nil || string(content) != "stale" {
		t.Fatalf("legacy skill mutated: %q, %v", content, err)
	}
	if content, err := os.ReadFile(retiredBinary); err != nil || string(content) != "retired binary" {
		t.Fatalf("legacy gate mutated retired binary: %q, %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(stellaHome, "bundles")); !os.IsNotExist(err) {
		t.Fatalf("legacy gate installed a bundle: %v", err)
	}
}

func TestSetupRunsLegacySkillGateBeforeEmbeddedPostgresMutation(t *testing.T) {
	stellaHome := setupCommandTestStellaHome(t)
	retired := filepath.Join(stellaHome, ".agents", "skills", "system", "kreuzberg")
	if err := os.MkdirAll(retired, 0o755); err != nil {
		t.Fatalf("create retired skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(retired, "SKILL.md"), []byte("stale"), 0o644); err != nil {
		t.Fatalf("write retired skill: %v", err)
	}

	if _, err := setup(t.Context(), config.ServerConfig{}, ""); err == nil {
		t.Fatal("setup accepted legacy custom skill")
	}
	for _, name := range []string{"postgres", "pg-runtime", "bundles"} {
		if _, err := os.Stat(filepath.Join(stellaHome, name)); !os.IsNotExist(err) {
			t.Fatalf("legacy gate allowed %s mutation: %v", name, err)
		}
	}
}

func TestRunHelp(t *testing.T) {
	app := newApp()
	err := app.Run([]string{"stellad", "--help"})
	if err != nil {
		t.Fatalf("run --help: %v", err)
	}
}

func TestRunHelpShort(t *testing.T) {
	app := newApp()
	err := app.Run([]string{"stellad", "-h"})
	if err != nil {
		t.Fatalf("run -h: %v", err)
	}
}

type blockingProjectReconciler struct{ started, release chan struct{} }

func (r blockingProjectReconciler) ReconcileProjectCoordinates(context.Context) (home.ProjectCoordinateReconcileResult, error) {
	close(r.started)
	<-r.release
	return home.ProjectCoordinateReconcileResult{}, nil
}

type blockingSkillReconciler struct{ started, release chan struct{} }

func (r blockingSkillReconciler) ReconcileStartup(context.Context) (skill.SkillStartupReconcileResult, error) {
	close(r.started)
	<-r.release
	return skill.SkillStartupReconcileResult{}, nil
}

func TestLegacyStorageReconciliationNeverBlocksSetup(t *testing.T) {
	projectStarted, skillStarted, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	scheduled := make(chan struct{})
	go func() {
		reconcileProjectCoordinatesInBackground(t.Context(), &wg, blockingProjectReconciler{started: projectStarted, release: release})
		reconcileSkillHomeInBackground(t.Context(), &wg, blockingSkillReconciler{started: skillStarted, release: release})
		close(scheduled)
	}()
	select {
	case <-scheduled:
	case <-time.After(time.Second):
		t.Fatal("background reconciliation blocked setup")
	}
	for name, started := range map[string]<-chan struct{}{"project": projectStarted, "Skill": skillStarted} {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("%s reconciliation did not start", name)
		}
	}
	close(release)
	wg.Wait()
}

func TestNativeServerUnsupportedPlatformFailsBeforeConfiguration(t *testing.T) {
	original := nativeServerGOOS
	nativeServerGOOS = "windows"
	t.Cleanup(func() { nativeServerGOOS = original })
	c := ucli.NewContext(ucli.NewApp(), flag.NewFlagSet("server", flag.ContinueOnError), nil)
	err := serverAction(c)
	if err == nil || !strings.Contains(err.Error(), "supported only on Linux and macOS") {
		t.Fatalf("unsupported server error = %v", err)
	}
	upgradeCtx := ucli.NewContext(ucli.NewApp(), flag.NewFlagSet("upgrade", flag.ContinueOnError), nil)
	if err := upgradeCommand().Action(upgradeCtx); err == nil || !strings.Contains(err.Error(), "supported only on Linux and macOS") {
		t.Fatalf("unsupported upgrade error = %v", err)
	}
}
