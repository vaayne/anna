package host

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/db/dbtest"
	"github.com/CherryHQ/stella/internal/plugin"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

func testHostSnapshot(t *testing.T, states map[string]bool, withoutConfig ...string) plugin.Snapshot {
	t.Helper()
	db := dbtest.New(t)
	catalog := plugin.NewCatalog()
	for id, enabled := range states {
		_, name, _ := strings.Cut(id, "/")
		if err := catalog.Register(plugin.Definition{ID: id, Namespace: name, DisplayName: name, Backend: plugin.BackendGo, Source: plugin.SourceBuiltin, ImplementationKey: id, Spec: []byte(`{}`), DefaultEnabled: enabled, Revision: 1}); err != nil {
			t.Fatal(err)
		}
	}
	service := plugin.NewService(db, nil, catalog, plugin.BackendPolicy{Transition: noopBackendTransition}, func(_ context.Context, fn func() error) error { return fn() })
	if err := service.SyncBuiltinDefaults(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, id := range withoutConfig {
		if _, err := db.Exec(t.Context(), `DELETE FROM plugin_config WHERE plugin_id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	authority, err := authz.NewSystemAuthority("host-test")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.ResolveSnapshot(t.Context(), authority, "")
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func noopBackendTransition(context.Context, pgx.Tx, authz.Authority, plugin.MutationKind, plugin.Definition, *plugin.Config, *plugin.Config) error {
	return nil
}

func TestRequiredHookCannotBypassSnapshotDenial(t *testing.T) {
	host := New(nil)
	host.RegisterPluginID("tool/denied")
	host.AddBeforeRun(pkgplugins.BeforeRunSpec{PluginID: "tool/denied", Name: "denied", Required: true, Run: func(context.Context, pkgplugins.BeforeRunContext) (pkgplugins.BeforeRunResult, error) {
		t.Fatal("disabled required hook ran")
		return pkgplugins.BeforeRunResult{}, nil
	}})
	snapshot := testHostSnapshot(t, map[string]bool{"tool/denied": false})
	got, err := host.BeforeRun(t.Context(), pkgplugins.BeforeRunContext{SystemPrompt: "base"}, snapshot)
	if err != nil || got.SystemPrompt != "base" {
		t.Fatalf("result = %+v, %v", got, err)
	}
}

func TestGoContributionRequiresPersistedConfigIdentity(t *testing.T) {
	snapshot := testHostSnapshot(t, map[string]bool{"tool/missing": true}, "tool/missing")
	_, enabled, err := snapshotState(snapshot, "tool/missing")
	if err == nil || enabled {
		t.Fatalf("definition fallback was executable: enabled=%v err=%v", enabled, err)
	}
}
