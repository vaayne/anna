package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/db/dbtest"
	pluginapi "github.com/CherryHQ/stella/internal/plugin"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

func TestUnifiedPluginRouteUsesDefinitionIdentity(t *testing.T) {
	for _, tc := range []struct {
		id, namespace, kind, name string
	}{
		{id: "tool/email", namespace: "email", kind: "tool", name: "email"},
		{id: "channel/feishu", namespace: "feishu", kind: "channel", name: "feishu"},
		{id: "standalone", namespace: "standalone", kind: "standalone", name: "standalone"},
	} {
		kind, name := unifiedPluginRoute(tc.id, tc.namespace)
		if kind != tc.kind || name != tc.name {
			t.Fatalf("unifiedPluginRoute(%q, %q) = %q/%q, want %q/%q", tc.id, tc.namespace, kind, name, tc.kind, tc.name)
		}
	}
}

func TestUnifiedPluginManagementHandlerRequiresUnifiedAccess(t *testing.T) {
	h := NewUnifiedPluginManagementHandler(nil)
	if _, err := h.List(t.Context(), SettingsPluginListInput{}); !errors.Is(err, pluginapi.ErrForbidden) {
		t.Fatalf("nil unified plugin access = %v, want forbidden", err)
	}
}

func TestSystemPluginEnabledInheritsDefaultAndRejectsAmbiguousState(t *testing.T) {
	definition := pluginapi.Definition{ID: "tool/demo", DefaultEnabled: true}
	if enabled, err := systemPluginEnabled(definition, nil); err != nil || !enabled {
		t.Fatalf("absent system state = %v, %v; want default enabled", enabled, err)
	}
	disabled := false
	if enabled, err := systemPluginEnabled(definition, []pluginapi.Config{{Enabled: &disabled}}); err != nil || enabled {
		t.Fatalf("explicit disabled state = %v, %v", enabled, err)
	}
	if _, err := systemPluginEnabled(definition, []pluginapi.Config{{}, {}}); !errors.Is(err, pluginapi.ErrConflict) {
		t.Fatalf("ambiguous system state = %v, want conflict", err)
	}
}

func unifiedPluginTestService(t *testing.T) *pluginapi.Service {
	t.Helper()
	db := dbtest.New(t)
	catalog := pluginapi.NewCatalog()
	definition := pluginapi.Definition{
		ID: "tool/demo", Namespace: "demo", DisplayName: "Demo", Backend: pluginapi.BackendGo,
		Source: pluginapi.SourceBuiltin, ImplementationKey: "tool/demo", Spec: []byte(`{}`),
		DefaultEnabled: true, Revision: 1,
	}
	if err := catalog.Register(definition); err != nil {
		t.Fatal(err)
	}
	service := pluginapi.NewService(db, nil, catalog, pluginapi.BackendPolicy{Transition: func(context.Context, pgx.Tx, authz.Authority, pluginapi.MutationKind, pluginapi.Definition, *pluginapi.Config, *pluginapi.Config) error {
		return nil
	}}, func(_ context.Context, fn func() error) error { return fn() })
	if err := service.SyncBuiltinDefaults(t.Context()); err != nil {
		t.Fatalf("sync plugin defaults: %v", err)
	}
	return service
}

func pluginToolContext(t *testing.T, authority authz.Authority) context.Context {
	t.Helper()
	return authz.WithUserID(authz.WithAuthority(context.Background(), authority), string(authority.UserID()))
}

func pluginListToolSpec(t *testing.T) SettingsPluginActionTool {
	t.Helper()
	for _, spec := range SettingsPluginActionTools() {
		if spec.Action == "list" {
			return spec
		}
	}
	t.Fatal("settings_plugin list spec missing")
	return SettingsPluginActionTool{}
}

func pluginActionToolSpec(t *testing.T, action string) SettingsPluginActionTool {
	t.Helper()
	for _, spec := range SettingsPluginActionTools() {
		if spec.Action == action {
			return spec
		}
	}
	t.Fatalf("settings_plugin %s spec missing", action)
	return SettingsPluginActionTool{}
}

func TestPluginManagementToolRejectsUnauthenticatedBeforeBindingService(t *testing.T) {
	called := false
	tool := NewPluginManagementTool(pluginListToolSpec(t), func() *pluginapi.Service {
		called = true
		return nil
	})
	_, err := tool.Execute(context.Background(), map[string]any{})
	if err == nil || !errors.Is(err, authz.ErrUnauthenticated) && !strings.Contains(err.Error(), "no user identity") {
		t.Fatalf("unauthenticated plugin tool error = %v", err)
	}
	if called {
		t.Fatal("plugin service was resolved before DirectAuthority")
	}
}

func TestPluginManagementToolNonAdminUsesCommonPermissionBoundary(t *testing.T) {
	service := unifiedPluginTestService(t)
	authority, err := authz.NewUserAuthority("10000000-0000-0000-0000-000000000002", false)
	if err != nil {
		t.Fatal(err)
	}
	tool := NewPluginManagementTool(pluginActionToolSpec(t, "disable"), func() *pluginapi.Service { return service })
	_, err = tool.Execute(pluginToolContext(t, authority), map[string]any{"kind": "tool", "name": "demo"})
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("non-admin plugin tool error = %v, want access denial", err)
	}
}

func TestUnifiedPluginManagementKeepsCommonConfigCAS(t *testing.T) {
	service := unifiedPluginTestService(t)
	authority, err := authz.NewUserAuthority("10000000-0000-0000-0000-000000000001", true)
	if err != nil {
		t.Fatal(err)
	}
	access, err := service.Begin(authority)
	if err != nil {
		t.Fatal(err)
	}
	configs, err := access.ListConfigs(t.Context(), "tool/demo", pluginapi.ScopeSystem, "")
	if err != nil || len(configs) != 1 {
		t.Fatalf("system configs = %d/%v, want one", len(configs), err)
	}
	initial := configs[0]
	disabled := false
	if _, err := access.UpdateConfig(t.Context(), initial.PluginID, initial.ID, initial.Revision, pluginapi.ConfigPatch{EnabledSet: true, Enabled: &disabled}); err != nil {
		t.Fatalf("first plugin toggle: %v", err)
	}
	if _, err := access.UpdateConfig(t.Context(), initial.PluginID, initial.ID, initial.Revision, pluginapi.ConfigPatch{EnabledSet: true, Enabled: &disabled}); !errors.Is(err, pluginapi.ErrConflict) {
		t.Fatalf("stale plugin toggle error = %v, want conflict", err)
	}
}
