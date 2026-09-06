package agent

// Issue #708 Section B: PoolManager one-shot pre-start binds vs the dynamic
// post-start reconfigure surface.
//
//   - AddBuiltinTool and the Bind* capability binders are sealed by StartAll:
//     they reject a nil value (missing), a second bind (duplicate), and any call
//     after StartAll (late).
//   - The dynamic reconfigure surface (ReloadPlugin*) stays available AFTER
//     StartAll — that distinction is asserted positively.

import (
	"context"
	"testing"

	cfgstore "github.com/CherryHQ/stella/cmd/stellad/store"
	"github.com/CherryHQ/stella/internal/authz"
	oauth "github.com/CherryHQ/stella/internal/connections/oauth"
	"github.com/CherryHQ/stella/internal/db/dbtest"
	"github.com/CherryHQ/stella/internal/platform/home"
	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/internal/vault"
	"github.com/CherryHQ/stella/pkg/tools"
)

type fakeBuiltinTool struct{ name string }

func (f fakeBuiltinTool) Definition() tools.Definition {
	return tools.Definition{Name: f.name}
}
func (f fakeBuiltinTool) Execute(context.Context, map[string]any) (string, error) { return "", nil }

type fakeMCPToolProvider struct{}

func (fakeMCPToolProvider) ToolsForSnapshot(context.Context, plugin.Snapshot) ([]tools.Tool, error) {
	return nil, nil
}

type fakeVaultEnvLoader struct{}

type fakePoolSessionAccess struct{}

func (fakePoolSessionAccess) Begin(context.Context, authz.Authority) (SessionAccess, error) {
	return nil, nil
}

func (fakeVaultEnvLoader) LoadEnvForAgent(context.Context, string, string) (map[string]string, error) {
	return nil, nil
}

func (fakeVaultEnvLoader) ListAmbientSecretMetas(context.Context, string, string) ([]vault.AmbientSecretMeta, error) {
	return nil, nil
}

func newPool(t *testing.T) *PoolManager {
	t.Helper()
	db := dbtest.New(t)
	workspaces, err := home.NewWorkspaceManager(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = workspaces.Close() })
	return NewPoolManager(cfgstore.NewDBStore(db), nil, WithHomeWorkspace(workspaces))
}

func startPool(t *testing.T, pm *PoolManager) {
	t.Helper()
	if err := pm.BindSessionAccess(fakePoolSessionAccess{}); err != nil {
		t.Fatalf("BindSessionAccess: %v", err)
	}
	if err := pm.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
}

func TestAddBuiltinToolGuards(t *testing.T) {
	pm := newPool(t)
	ctx := context.Background()

	if err := pm.AddBuiltinTool(ctx, nil); err == nil {
		t.Error("AddBuiltinTool(nil) should error")
	}
	if err := pm.AddBuiltinTool(ctx, fakeBuiltinTool{name: "x"}); err != nil {
		t.Fatalf("first AddBuiltinTool: %v", err)
	}
	if err := pm.AddBuiltinTool(ctx, fakeBuiltinTool{name: "x"}); err == nil {
		t.Error("duplicate AddBuiltinTool should error")
	}
	startPool(t, pm)
	if err := pm.AddBuiltinTool(ctx, fakeBuiltinTool{name: "y"}); err == nil {
		t.Error("AddBuiltinTool after StartAll should error")
	}
}

func TestBindOAuthRegistryGuards(t *testing.T) {
	pm := newPool(t)
	if err := pm.BindOAuthRegistry(nil); err == nil {
		t.Error("BindOAuthRegistry(nil) should error")
	}
	if err := pm.BindOAuthRegistry(oauth.NewProviderRegistry()); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	if err := pm.BindOAuthRegistry(oauth.NewProviderRegistry()); err == nil {
		t.Error("duplicate BindOAuthRegistry should error")
	}
	pm2 := newPool(t)
	startPool(t, pm2)
	if err := pm2.BindOAuthRegistry(oauth.NewProviderRegistry()); err == nil {
		t.Error("BindOAuthRegistry after StartAll should error")
	}
}

func TestBindSessionAccessRequired(t *testing.T) {
	pm := newPool(t)
	if err := pm.StartAll(context.Background()); err == nil {
		t.Error("StartAll without SessionAccess should error")
	}
	if err := pm.BindSessionAccess(fakePoolSessionAccess{}); err != nil {
		t.Fatalf("BindSessionAccess after rejected StartAll: %v", err)
	}
	if err := pm.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll after bind: %v", err)
	}
	if err := pm.BindSessionAccess(fakePoolSessionAccess{}); err == nil {
		t.Error("BindSessionAccess after StartAll should error")
	}
}

func TestBindMCPToolProviderGuards(t *testing.T) {
	pm := newPool(t)
	if err := pm.BindMCPToolProvider(nil); err == nil {
		t.Error("BindMCPToolProvider(nil) should error")
	}
	if err := pm.BindMCPToolProvider(fakeMCPToolProvider{}); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	if err := pm.BindMCPToolProvider(fakeMCPToolProvider{}); err == nil {
		t.Error("duplicate BindMCPToolProvider should error")
	}
	pm2 := newPool(t)
	startPool(t, pm2)
	if err := pm2.BindMCPToolProvider(fakeMCPToolProvider{}); err == nil {
		t.Error("BindMCPToolProvider after StartAll should error")
	}
}

func TestBindVaultEnvLoaderGuards(t *testing.T) {
	pm := newPool(t)
	if err := pm.BindVaultEnvLoader(nil); err == nil {
		t.Error("BindVaultEnvLoader(nil) should error")
	}
	if err := pm.BindVaultEnvLoader(fakeVaultEnvLoader{}); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	if err := pm.BindVaultEnvLoader(fakeVaultEnvLoader{}); err == nil {
		t.Error("duplicate BindVaultEnvLoader should error")
	}
	pm2 := newPool(t)
	startPool(t, pm2)
	if err := pm2.BindVaultEnvLoader(fakeVaultEnvLoader{}); err == nil {
		t.Error("BindVaultEnvLoader after StartAll should error")
	}
}

func TestStartAllIsOneShot(t *testing.T) {
	pm := newPool(t)
	startPool(t, pm)
	if err := pm.StartAll(context.Background()); err == nil {
		t.Error("second StartAll should error")
	}
}

// TestReconfigureAvailableAfterStart is the positive half of the distinction:
// the dynamic plugin reconfigure surface keeps working after StartAll (here with
// no live agents, so each is a successful no-op), unlike the sealed binds above.
func TestReconfigureAvailableAfterStart(t *testing.T) {
	pm := newPool(t)
	startPool(t, pm)
	ctx := context.Background()
	if err := pm.ReloadPluginTools(ctx); err != nil {
		t.Errorf("ReloadPluginTools after StartAll: %v", err)
	}
	if err := pm.ReloadPluginHooks(ctx); err != nil {
		t.Errorf("ReloadPluginHooks after StartAll: %v", err)
	}
	if err := pm.ReloadProviders(ctx); err != nil {
		t.Errorf("ReloadProviders after StartAll: %v", err)
	}
}
