package db

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/CherryHQ/stella/internal/plugin"
)

func TestUnifiedPluginSnapshotFiltersDormantAndForeignDefinitions(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	userA := insertPluginUser(t, db, "snapshot-a@example.test", false)
	userB := insertPluginUser(t, db, "snapshot-b@example.test", false)

	builtin := pluginDefinition("builtin/shared", "shared", true)
	dormant := pluginDefinition("builtin/dormant", "dormant", true)
	custom := plugin.Definition{
		ID: "custom/shared-a", Namespace: "shared", DisplayName: "Shared A",
		Backend: plugin.BackendMCP, Source: plugin.SourceCustom, ImplementationKey: "mcp-a",
		Spec: json.RawMessage(`{"schema":1}`), Revision: 1, CreatorUserID: string(userA.UserID()),
	}
	hidden := plugin.Definition{
		ID: "custom/hidden", Namespace: "hidden", DisplayName: "Hidden",
		Backend: plugin.BackendMCP, Source: plugin.SourceCustom, ImplementationKey: "mcp-hidden",
		Spec: json.RawMessage(`{}`), Revision: 1, CreatorUserID: string(userA.UserID()),
	}
	foreign := plugin.Definition{
		ID: "custom/foreign", Namespace: "foreign", DisplayName: "Foreign",
		Backend: plugin.BackendMCP, Source: plugin.SourceCustom, ImplementationKey: "mcp-foreign",
		Spec: json.RawMessage(`{}`), Revision: 1, CreatorUserID: string(userB.UserID()),
	}
	negative := plugin.Definition{
		ID: "custom/negative", Namespace: "negative", DisplayName: "Negative",
		Backend: plugin.BackendMCP, Source: plugin.SourceCustom, ImplementationKey: "mcp-negative",
		Spec: json.RawMessage(`{}`), Revision: 1, CreatorUserID: string(userA.UserID()),
	}
	negativePayload := plugin.Definition{
		ID: "custom/negative-payload", Namespace: "negative", DisplayName: "Negative Payload",
		Backend: plugin.BackendMCP, Source: plugin.SourceCustom, ImplementationKey: "mcp-negative-payload",
		Spec: json.RawMessage(`{}`), Revision: 1, CreatorUserID: string(userA.UserID()),
	}
	privateNegative := plugin.Definition{
		ID: "custom/private-negative", Namespace: "private-negative", DisplayName: "Private Negative",
		Backend: plugin.BackendMCP, Source: plugin.SourceCustom, ImplementationKey: "mcp-private-negative",
		Spec: json.RawMessage(`{}`), Revision: 1, CreatorUserID: string(userB.UserID()),
	}
	sharedDenied := plugin.Definition{
		ID: "custom/shared-denied", Namespace: "shared-denied", DisplayName: "Shared Denied",
		Backend: plugin.BackendMCP, Source: plugin.SourceCustom, ImplementationKey: "mcp-shared-denied",
		Spec: json.RawMessage(`{}`), Revision: 1, CreatorUserID: string(userA.UserID()),
	}

	for _, def := range []plugin.Definition{builtin, dormant, custom, hidden, foreign, negative, negativePayload, privateNegative, sharedDenied} {
		if _, err := db.Exec(ctx, `
			INSERT INTO plugin_definition (id, namespace, display_name, backend, source, implementation_key, spec, default_enabled, revision, creator_user_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10, '')::uuid)
		`, def.ID, def.Namespace, def.DisplayName, def.Backend, def.Source, def.ImplementationKey, def.Spec, def.DefaultEnabled, def.Revision, def.CreatorUserID); err != nil {
			t.Fatalf("insert definition %s: %v", def.ID, err)
		}
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin_config (plugin_id, namespace, scope, user_id, enabled, config)
		VALUES ($1, $2, 'user', $3, true, '{}'::jsonb)
	`, custom.ID, custom.Namespace, userA.UserID()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin_config (plugin_id, namespace, scope, user_id, enabled, config)
		VALUES ($1, $2, 'user', $3, true, '{}'::jsonb)
	`, foreign.ID, foreign.Namespace, userB.UserID()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin_config (plugin_id, namespace, scope, user_id, enabled, config)
		VALUES ($1, $2, 'user', $3, false, NULL)
	`, negative.ID, negative.Namespace, userA.UserID()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin_config (plugin_id, namespace, scope, user_id, enabled, config)
		VALUES ($1, $2, 'user', $3, true, '{}'::jsonb)
	`, negativePayload.ID, negativePayload.Namespace, userA.UserID()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin_config (plugin_id, namespace, scope, enabled, config)
		VALUES ($1, $2, 'system', false, NULL)
	`, privateNegative.ID, privateNegative.Namespace); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin_config (plugin_id, namespace, scope, enabled, config)
		VALUES ($1, $2, 'system', false, '{}'::jsonb)
	`, sharedDenied.ID, sharedDenied.Namespace); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin_config (plugin_id, namespace, scope, enabled, config)
		VALUES ($1, $2, 'system', true, '{}'::jsonb)
	`, dormant.ID, dormant.Namespace); err != nil {
		t.Fatalf("insert dormant config: %v", err)
	}

	catalog := plugin.NewCatalog()
	if err := catalog.Register(builtin); err != nil {
		t.Fatal(err)
	}
	service := plugin.NewService(db, nil, catalog, plugin.BackendPolicy{Validate: noopPluginValidator, Transition: noopBackendTransition}, inlinePluginMutationFence)
	snapshot, err := service.ResolveSnapshot(ctx, userA, "")
	if err != nil {
		t.Fatal(err)
	}

	selected, ok := snapshot.Get(custom.ID)
	if !ok || selected.Effective.PluginID != custom.ID || selected.Config == nil {
		t.Fatalf("custom winner = %#v, found=%v", selected, ok)
	}
	loser, ok := snapshot.Get(builtin.ID)
	if !ok || loser.Effective.PluginID != builtin.ID || !loser.Effective.IsEffectivelyEnabled {
		t.Fatalf("same-namespace builtin by-id result = %#v, found=%v", loser, ok)
	}
	if _, ok := snapshot.Get(hidden.ID); ok {
		t.Fatal("custom definition without owned payload was exposed")
	}
	if _, ok := snapshot.Get(foreign.ID); ok {
		t.Fatal("foreign custom definition was exposed")
	}
	if _, ok := snapshot.Get(privateNegative.ID); ok {
		t.Fatal("ownerless system-negative custom definition was exposed")
	}
	if _, err := snapshot.Resolve(privateNegative.ID); !errors.Is(err, plugin.ErrNotFound) {
		t.Fatalf("ownerless system-negative Resolve = %v, want not found", err)
	}
	for _, def := range snapshot.Definitions() {
		if def.ID == privateNegative.ID {
			t.Fatal("ownerless system-negative custom definition was listed")
		}
	}
	if _, ok := snapshot.Get("builtin/dormant"); ok {
		t.Fatal("builtin absent from shipped catalog was exposed")
	}
	negativeResult, ok := snapshot.Get(negative.ID)
	if !ok || negativeResult.Effective.IsEffectivelyEnabled {
		t.Fatalf("custom own-negative by-id result = %#v, found=%v", negativeResult, ok)
	}
	sharedDeniedResult, ok := snapshot.Get(sharedDenied.ID)
	if !ok || sharedDeniedResult.Effective.IsEffectivelyEnabled {
		t.Fatalf("shared payload-negative by-id result = %#v, found=%v", sharedDeniedResult, ok)
	}
	resolved, err := snapshot.ResolveNamespace(sharedDenied.Namespace)
	if err != nil || resolved.PluginID != sharedDenied.ID || resolved.IsEffectivelyEnabled {
		t.Fatalf("shared payload-negative namespace = %#v, err=%v", resolved, err)
	}
	resolved, err = snapshot.ResolveNamespace(custom.Namespace)
	if err != nil || resolved.PluginID != custom.ID {
		t.Fatalf("shared namespace = %#v, err=%v", resolved, err)
	}
	resolved, err = snapshot.ResolveNamespace(negative.Namespace)
	if err != nil || resolved.PluginID != negativePayload.ID || !resolved.IsEffectivelyEnabled {
		t.Fatalf("negative namespace = %#v, err=%v", resolved, err)
	}
}
