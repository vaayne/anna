package db

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/plugin"
)

func pluginDefinition(id, namespace string, enabled bool) plugin.Definition {
	return plugin.Definition{
		ID: id, Namespace: namespace, DisplayName: id,
		Backend: plugin.BackendGo, Source: plugin.SourceBuiltin,
		ImplementationKey: id, Spec: json.RawMessage(`{"schema":1}`),
		DefaultEnabled: enabled, Revision: 1,
	}
}

func insertPluginUser(t *testing.T, db *pgxpool.Pool, email string, admin bool) authz.Authority {
	t.Helper()
	var id string
	if err := db.QueryRow(t.Context(), `INSERT INTO auth_user (email) VALUES ($1) RETURNING id`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	authority, err := authz.NewUserAuthority(authz.UserID(id), admin)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func noopPluginValidator(context.Context, plugin.Definition, plugin.Config, []string) error {
	return nil
}

func inlinePluginMutationFence(_ context.Context, mutate func() error) error {
	return mutate()
}

func noopBackendTransition(context.Context, pgx.Tx, authz.Authority, plugin.MutationKind, plugin.Definition, *plugin.Config, *plugin.Config) error {
	return nil
}

func TestUnifiedPluginConfigConstraints(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	insertDefinition := func(id, namespace string) {
		t.Helper()
		if _, err := db.Exec(ctx, `
			INSERT INTO plugin_definition (id, namespace, display_name, backend, source, implementation_key, spec)
			VALUES ($1, $2, $1, 'go', 'builtin', $1, '{}'::jsonb)
		`, id, namespace); err != nil {
			t.Fatal(err)
		}
	}
	for _, pair := range [][2]string{
		{"builtin/owner", "constraint-owner"},
		{"builtin/payload", "constraint-payload"},
		{"builtin/refs", "constraint-refs"},
		{"builtin/null", "constraint-null"},
		{"builtin/shared-payload", "constraint-shared"},
		{"builtin/shared-negative", "constraint-shared"},
	} {
		insertDefinition(pair[0], pair[1])
	}

	assertConstraint := func(name, expected, statement string, args ...any) {
		t.Helper()
		_, err := db.Exec(ctx, statement, args...)
		if err == nil {
			t.Fatalf("%s: invalid row was accepted", name)
		}
		pgErr := &pgconn.PgError{}
		ok := errors.As(err, &pgErr)
		if !ok || pgErr.Code != "23514" || pgErr.ConstraintName != expected {
			t.Fatalf("%s: error = %T %v, want check %s", name, err, err, expected)
		}
	}
	assertConstraint("owner tuple", "plugin_config_scope_owner_check", `
		INSERT INTO plugin_config (plugin_id, namespace, scope, enabled, config)
		VALUES ('builtin/owner', 'constraint-owner', 'user', false, NULL)
	`)
	assertConstraint("enabled payload", "plugin_config_negative_check", `
		INSERT INTO plugin_config (plugin_id, namespace, scope, enabled, config)
		VALUES ('builtin/payload', 'constraint-payload', 'system', true, NULL)
	`)
	assertConstraint("negative refs", "plugin_config_negative_refs_check", `
		INSERT INTO plugin_config (plugin_id, namespace, scope, enabled, config, credential_refs)
		VALUES ('builtin/refs', 'constraint-refs', 'system', false, NULL, '{"vault":"key"}'::jsonb)
	`)
	assertConstraint("JSON null payload", "plugin_config_config_object_check", `
		INSERT INTO plugin_config (plugin_id, namespace, scope, enabled, config)
		VALUES ('builtin/null', 'constraint-null', 'system', false, 'null'::jsonb)
	`)
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin_config (plugin_id, namespace, scope, enabled, config)
		VALUES ('builtin/shared-payload', 'constraint-shared', 'system', true, '{}'::jsonb)
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin_config (plugin_id, namespace, scope, enabled, config)
		VALUES ('builtin/shared-negative', 'constraint-shared', 'system', false, NULL)
	`); err != nil {
		t.Fatalf("negative row claimed namespace: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin_config (plugin_id, namespace, scope, enabled, config, credential_refs)
		VALUES ('builtin/refs', 'constraint-refs', 'system', true, '{}'::jsonb, '{"vault":"allowed"}'::jsonb)
	`); err != nil {
		t.Fatalf("payload credential refs rejected: %v", err)
	}
}

func syncPluginCatalog(t *testing.T, db *pgxpool.Pool, definitions ...plugin.Definition) (*plugin.Service, *plugin.Catalog) {
	t.Helper()
	catalog := plugin.NewCatalog()
	for _, def := range definitions {
		if err := catalog.Register(def); err != nil {
			t.Fatal(err)
		}
	}
	service := plugin.NewService(db, nil, catalog, plugin.BackendPolicy{Validate: noopPluginValidator, Transition: noopBackendTransition}, inlinePluginMutationFence)
	if err := service.SyncBuiltinDefaults(t.Context()); err != nil {
		t.Fatal(err)
	}
	return service, catalog
}

func TestUnifiedPluginFreshSyncAndDefaultPreservesState(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	def := pluginDefinition("builtin/fresh", "fresh", true)
	service, _ := syncPluginCatalog(t, db, def)

	var definitionCreated, definitionUpdated time.Time
	if err := db.QueryRow(ctx, `SELECT created_at, updated_at FROM plugin_definition WHERE id = $1`, def.ID).Scan(&definitionCreated, &definitionUpdated); err != nil {
		t.Fatal(err)
	}
	var configID string
	var created, updated time.Time
	var enabled pgtype.Bool
	var payload, refs []byte
	var revision int64
	if err := db.QueryRow(ctx, `
		SELECT id, enabled, config, credential_refs, revision, created_at, updated_at
		FROM plugin_config WHERE plugin_id = $1 AND scope = 'system'
	`, def.ID).Scan(&configID, &enabled, &payload, &refs, &revision, &created, &updated); err != nil {
		t.Fatal(err)
	}
	if enabled.Valid || string(payload) != `{}` || string(refs) != `{}` || revision != 1 {
		t.Fatalf("fresh system projection = enabled=%v payload=%s refs=%s revision=%d", enabled, payload, refs, revision)
	}

	if _, err := db.Exec(ctx, `
		UPDATE plugin_config
		SET enabled = false, config = '{"pin":true}'::jsonb, credential_refs = '{"vault":"key"}'::jsonb, revision = 7
		WHERE id = $1
	`, configID); err != nil {
		t.Fatal(err)
	}
	if err := service.SyncBuiltinDefaults(ctx); err != nil {
		t.Fatal(err)
	}

	var gotID string
	var gotEnabled bool
	var gotPayload, gotRefs []byte
	var gotRevision int64
	var gotCreated, gotUpdated time.Time
	if err := db.QueryRow(ctx, `
		SELECT id, enabled, config, credential_refs, revision, created_at, updated_at
		FROM plugin_config WHERE plugin_id = $1 AND scope = 'system'
	`, def.ID).Scan(&gotID, &gotEnabled, &gotPayload, &gotRefs, &gotRevision, &gotCreated, &gotUpdated); err != nil {
		t.Fatal(err)
	}
	if gotID != configID || gotEnabled || !jsonEqual(gotPayload, []byte(`{"pin":true}`)) || !jsonEqual(gotRefs, []byte(`{"vault":"key"}`)) || gotRevision != 7 || !gotCreated.Equal(created) || !gotUpdated.Equal(updated) {
		t.Fatalf("sync rewrote pinned projection: id=%s enabled=%v payload=%s refs=%s revision=%d", gotID, gotEnabled, gotPayload, gotRefs, gotRevision)
	}
	var gotDefinitionCreated, gotDefinitionUpdated time.Time
	if err := db.QueryRow(ctx, `SELECT created_at, updated_at FROM plugin_definition WHERE id = $1`, def.ID).Scan(&gotDefinitionCreated, &gotDefinitionUpdated); err != nil {
		t.Fatal(err)
	}
	if !gotDefinitionCreated.Equal(definitionCreated) || !gotDefinitionUpdated.Equal(definitionUpdated) {
		t.Fatalf("repeated sync changed definition timestamps: %s/%s -> %s/%s", definitionCreated, definitionUpdated, gotDefinitionCreated, gotDefinitionUpdated)
	}
}

func TestUnifiedPluginSyncFailureRollsBackEarlierDefinitions(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin_definition (id, namespace, display_name, backend, source, implementation_key, spec)
		VALUES ('builtin/conflict', 'conflict', 'old', 'mcp', 'custom', 'old', '{}'::jsonb)
	`); err != nil {
		t.Fatal(err)
	}
	catalog := plugin.NewCatalog()
	for _, def := range []plugin.Definition{
		pluginDefinition("builtin/good", "good", true),
		pluginDefinition("builtin/conflict", "conflict", true),
	} {
		if err := catalog.Register(def); err != nil {
			t.Fatal(err)
		}
	}
	service := plugin.NewService(db, nil, catalog, plugin.BackendPolicy{Validate: noopPluginValidator, Transition: noopBackendTransition}, inlinePluginMutationFence)
	if err := service.SyncBuiltinDefaults(ctx); err == nil {
		t.Fatal("sync accepted an incompatible existing definition")
	}
	var definitions, configs int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_definition WHERE id = 'builtin/good'`).Scan(&definitions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_config WHERE plugin_id = 'builtin/good'`).Scan(&configs); err != nil {
		t.Fatal(err)
	}
	if definitions != 0 || configs != 0 {
		t.Fatalf("failed sync left partial state: definitions=%d configs=%d", definitions, configs)
	}
}

func TestUnifiedPluginAccessOwnerCASAndReset(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	def := pluginDefinition("builtin/owner", "owner", true)
	otherDef := pluginDefinition("builtin/other", "other", true)
	service, _ := syncPluginCatalog(t, db, def, otherDef)
	userA := insertPluginUser(t, db, "plugin-a@example.test", false)
	userB := insertPluginUser(t, db, "plugin-b@example.test", false)
	accessA, err := service.Begin(userA)
	if err != nil {
		t.Fatal(err)
	}
	value := true
	created, err := accessA.CreateConfig(ctx, plugin.Config{
		PluginID: def.ID, Scope: plugin.ScopeUser, Enabled: &value,
		Payload: json.RawMessage(`{"version":"1","env":"keep","owned":"mine"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	accessB, err := service.Begin(userB)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		fn   func() error
	}{
		{"other user read", func() error { _, err := accessB.GetConfig(ctx, def.ID, created.ID); return err }},
		{"wrong plugin read", func() error { _, err := accessA.GetConfig(ctx, otherDef.ID, created.ID); return err }},
		{"wrong plugin delete", func() error { return accessA.DeleteConfig(ctx, otherDef.ID, created.ID, 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fn(); !errors.Is(err, plugin.ErrNotFound) {
				t.Fatalf("error = %v, want not found", err)
			}
		})
	}

	updated, err := accessA.UpdateConfig(ctx, def.ID, created.ID, 1, plugin.ConfigPatch{
		PayloadSet: true,
		Payload:    json.RawMessage(`{"version":"2","owned":null}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 2 {
		t.Fatalf("revision after patch = %d, want 2", updated.Revision)
	}
	updated, err = accessA.UpdateConfig(ctx, def.ID, created.ID, 2, plugin.ConfigPatch{ResetFields: []string{"env"}})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 3 || string(updated.Payload) == `{"version":"2","env":"keep","owned":null}` {
		t.Fatalf("reset patch = revision %d payload %s", updated.Revision, updated.Payload)
	}
	if _, err := accessA.UpdateConfig(ctx, def.ID, created.ID, 1, plugin.ConfigPatch{EnabledSet: true, Enabled: boolPtr(false)}); !errors.Is(err, plugin.ErrConflict) {
		t.Fatalf("stale update = %v, want conflict", err)
	}
	var revision int64
	if err := db.QueryRow(ctx, `SELECT revision FROM plugin_config WHERE id = $1`, created.ID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if revision != 3 {
		t.Fatalf("stale CAS mutated revision to %d", revision)
	}

	admin := insertPluginUser(t, db, "plugin-admin@example.test", true)
	adminAccess, err := service.Begin(admin)
	if err != nil {
		t.Fatal(err)
	}
	var systemID string
	if err := db.QueryRow(ctx, `SELECT id FROM plugin_config WHERE plugin_id = $1 AND scope = 'system'`, def.ID).Scan(&systemID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE plugin_config SET enabled = false, revision = 4 WHERE id = $1`, systemID); err != nil {
		t.Fatal(err)
	}
	reset, err := adminAccess.ResetBuiltinConfig(ctx, def.ID, systemID, 4)
	if err != nil {
		t.Fatal(err)
	}
	if reset.Enabled != nil || string(reset.Payload) != `{}` || reset.Revision != 5 {
		t.Fatalf("reset builtin = %#v", reset)
	}
}

func TestUnifiedPluginPayloadValidatorSeparatesNegativeAndReady(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	def := pluginDefinition("builtin/shape", "shape", true)
	catalog := plugin.NewCatalog()
	if err := catalog.Register(def); err != nil {
		t.Fatal(err)
	}
	var validatorCalls int
	validator := func(_ context.Context, _ plugin.Definition, _ plugin.Config, _ []string) error {
		validatorCalls++
		return plugin.ErrInvalidConfig
	}
	service := plugin.NewService(db, nil, catalog, plugin.BackendPolicy{Validate: validator, Transition: noopBackendTransition}, inlinePluginMutationFence)
	if err := service.SyncBuiltinDefaults(ctx); err != nil {
		t.Fatal(err)
	}
	user := insertPluginUser(t, db, "plugin-shape@example.test", false)
	access, err := service.Begin(user)
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	if _, err := access.CreateConfig(ctx, plugin.Config{PluginID: def.ID, Scope: plugin.ScopeUser, Enabled: &disabled}); err != nil {
		t.Fatalf("negative config without required connection fields: %v", err)
	}
	if validatorCalls != 0 {
		t.Fatalf("disabled negative config invoked full validator %d times", validatorCalls)
	}
	user2 := insertPluginUser(t, db, "plugin-shape-enabled@example.test", false)
	access2, err := service.Begin(user2)
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	if _, err := access2.CreateConfig(ctx, plugin.Config{PluginID: def.ID, Scope: plugin.ScopeUser, Enabled: &enabled, Payload: json.RawMessage(`{}`)}); !errors.Is(err, plugin.ErrInvalidConfig) {
		t.Fatalf("enabled config without required connection = %v, want invalid config", err)
	}
	if validatorCalls != 1 {
		t.Fatalf("enabled config validator calls = %d, want 1", validatorCalls)
	}
}

func TestUnifiedPluginSafetyCallbackRunsForWritesUnderSystemDeny(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	def := pluginDefinition("builtin/deny", "deny", true)
	catalog := plugin.NewCatalog()
	if err := catalog.Register(def); err != nil {
		t.Fatal(err)
	}
	var calls []plugin.Config
	validator := func(_ context.Context, _ plugin.Definition, config plugin.Config, _ []string) error {
		calls = append(calls, config)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(config.Payload, &fields); err != nil {
			return plugin.ErrInvalidConfig
		}
		if _, safe := fields["safe"]; safe {
			return nil
		}
		return plugin.ErrInvalidConfig
	}
	service := plugin.NewService(db, nil, catalog, plugin.BackendPolicy{Validate: validator, Transition: noopBackendTransition}, inlinePluginMutationFence)
	if err := service.SyncBuiltinDefaults(ctx); err != nil {
		t.Fatal(err)
	}
	closingUser := insertPluginUser(t, db, "plugin-close@example.test", false)
	closingAccess, err := service.Begin(closingUser)
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	created, err := closingAccess.CreateConfig(ctx, plugin.Config{
		PluginID: def.ID, Scope: plugin.ScopeUser, Enabled: &enabled,
		Payload: json.RawMessage(`{"safe":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	callsAfterCreate := len(calls)
	if _, err := db.Exec(ctx, `UPDATE plugin_config SET enabled = false, revision = revision + 1 WHERE plugin_id = $1 AND scope = 'system'`, def.ID); err != nil {
		t.Fatal(err)
	}
	disabled := false
	if _, err := closingAccess.UpdateConfig(ctx, def.ID, created.ID, created.Revision, plugin.ConfigPatch{EnabledSet: true, Enabled: &disabled}); err != nil {
		t.Fatalf("pure close under system deny: %v", err)
	}
	if len(calls) != callsAfterCreate {
		t.Fatalf("pure enabled=false close invoked callback: before=%d after=%d", callsAfterCreate, len(calls))
	}

	maliciousUser := insertPluginUser(t, db, "plugin-deny-malicious@example.test", false)
	maliciousAccess, err := service.Begin(maliciousUser)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := maliciousAccess.CreateConfig(ctx, plugin.Config{
		PluginID: def.ID, Scope: plugin.ScopeUser, Enabled: &enabled,
		Payload: json.RawMessage(`{"malicious_secret":"blocked"}`),
	}); !errors.Is(err, plugin.ErrInvalidConfig) {
		t.Fatalf("true write under system deny = %v, want invalid config", err)
	}
	if len(calls) == 0 || calls[len(calls)-1].Enabled == nil || *calls[len(calls)-1].Enabled {
		t.Fatalf("system deny was not passed to callback: %#v", calls)
	}

	explicitFalseUser := insertPluginUser(t, db, "plugin-deny-explicit@example.test", false)
	explicitFalseAccess, err := service.Begin(explicitFalseUser)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := explicitFalseAccess.CreateConfig(ctx, plugin.Config{
		PluginID: def.ID, Scope: plugin.ScopeUser, Enabled: &disabled,
		Payload:        json.RawMessage(`{"malicious_secret":"blocked"}`),
		CredentialRefs: json.RawMessage(`{"ref":"secret"}`),
	}); !errors.Is(err, plugin.ErrInvalidConfig) {
		t.Fatalf("explicit false malicious write = %v, want invalid config", err)
	}
	var rows int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_config WHERE plugin_id = $1 AND user_id IN ($2, $3)`, def.ID, maliciousUser.UserID(), explicitFalseUser.UserID()).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("rejected malicious writes persisted %d rows", rows)
	}
}

func TestUnifiedPluginNegativeResetKeepsNullPayloadAndNoNamespaceClaim(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	first := pluginDefinition("builtin/negative-one", "negative-shared", true)
	second := pluginDefinition("builtin/negative-two", "negative-shared", true)
	syncPluginCatalog(t, db, first)
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin_definition (id, namespace, display_name, backend, source, implementation_key, spec)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, second.ID, second.Namespace, second.DisplayName, second.Backend, second.Source, second.ImplementationKey, second.Spec); err != nil {
		t.Fatal(err)
	}
	catalog := plugin.NewCatalog()
	for _, definition := range []plugin.Definition{first, second} {
		if err := catalog.Register(definition); err != nil {
			t.Fatal(err)
		}
	}
	service := plugin.NewService(db, nil, catalog, plugin.BackendPolicy{Validate: noopPluginValidator, Transition: noopBackendTransition}, inlinePluginMutationFence)
	user := insertPluginUser(t, db, "plugin-negative@example.test", false)
	access, err := service.Begin(user)
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	negative, err := access.CreateConfig(ctx, plugin.Config{PluginID: first.ID, Scope: plugin.ScopeUser, Enabled: &disabled})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := access.UpdateConfig(ctx, first.ID, negative.ID, negative.Revision, plugin.ConfigPatch{ResetFields: []string{"missing"}}); !errors.Is(err, plugin.ErrInvalidConfig) {
		t.Fatalf("negative reset = %v, want invalid config", err)
	}
	var payload any
	var revision int64
	if err := db.QueryRow(ctx, `SELECT config, revision FROM plugin_config WHERE id = $1`, negative.ID).Scan(&payload, &revision); err != nil {
		t.Fatal(err)
	}
	if payload != nil || revision != negative.Revision {
		t.Fatalf("rejected negative reset mutated row: payload=%#v revision=%d", payload, revision)
	}
	enabled := true
	claimed, err := access.CreateConfig(ctx, plugin.Config{
		PluginID: second.ID, Scope: plugin.ScopeUser, Enabled: &enabled,
		Payload: json.RawMessage(`{"connection":"second"}`),
	})
	if err != nil {
		t.Fatalf("payload config after negative reset: %v", err)
	}
	if claimed.Namespace != first.Namespace {
		t.Fatalf("shared namespace changed: %q", claimed.Namespace)
	}
}

func TestUnifiedPluginSharedCustomVisibleWithoutSystemPayload(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin_definition (id, namespace, display_name, backend, source, implementation_key, spec)
		VALUES ('custom/shared', 'shared', 'Shared', 'mcp', 'custom', 'mcp', '{}'::jsonb)
	`); err != nil {
		t.Fatal(err)
	}
	var configID string
	if err := db.QueryRow(ctx, `
		INSERT INTO plugin_config (plugin_id, namespace, scope, enabled, config)
		VALUES ('custom/shared', 'shared', 'system', true, '{"secret":"do-not-return"}'::jsonb)
		RETURNING id
	`).Scan(&configID); err != nil {
		t.Fatal(err)
	}
	service := plugin.NewService(db, nil, plugin.NewCatalog(), plugin.BackendPolicy{Validate: noopPluginValidator, Transition: noopBackendTransition}, inlinePluginMutationFence)
	user := insertPluginUser(t, db, "plugin-shared@example.test", false)
	access, err := service.Begin(user)
	if err != nil {
		t.Fatal(err)
	}
	def, err := access.GetDefinition(ctx, "custom/shared")
	if err != nil || def.ID != "custom/shared" {
		t.Fatalf("shared definition lookup = %#v, %v", def, err)
	}
	definitions, err := access.ListDefinitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range definitions {
		if candidate.ID == "custom/shared" {
			found = true
		}
	}
	if !found {
		t.Fatal("shared custom definition was hidden")
	}
	if _, err := access.GetConfig(ctx, "custom/shared", configID); !errors.Is(err, plugin.ErrNotFound) {
		t.Fatalf("system payload read = %v, want not found", err)
	}
}

func TestUnifiedPluginCustomIdentityValidationAndNamespaceRollback(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	user := insertPluginUser(t, db, "plugin-custom@example.test", false)
	catalog := plugin.NewCatalog()
	if err := catalog.Register(pluginDefinition("builtin/taken", "taken", true)); err != nil {
		t.Fatal(err)
	}
	service := plugin.NewService(db, nil, catalog, plugin.BackendPolicy{Validate: noopPluginValidator, Transition: noopBackendTransition}, inlinePluginMutationFence)
	access, err := service.Begin(user)
	if err != nil {
		t.Fatal(err)
	}
	createdDef, createdConfig, err := access.CreateCustom(ctx,
		plugin.Definition{Namespace: "requested_name", DisplayName: "Remote", Backend: plugin.BackendMCP, Spec: json.RawMessage(`{"description":"safe"}`)},
		plugin.Config{Scope: plugin.ScopeUser, Enabled: boolPtr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if createdDef.Source != plugin.SourceCustom || createdDef.DefaultEnabled || createdDef.Revision != 1 || createdDef.CreatorUserID != string(user.UserID()) || createdDef.Namespace != "requested_name" || createdConfig.PluginID != createdDef.ID || createdDef.ID != "custom/"+createdConfig.ID || createdConfig.Revision != 1 {
		t.Fatalf("custom identity = %#v / %#v", createdDef, createdConfig)
	}
	displayName, description := "Renamed", "Updated description"
	updatedDef, err := access.UpdateDefinition(ctx, createdDef.ID, createdDef.Revision, plugin.DefinitionPatch{DisplayName: &displayName, Description: &description})
	if err != nil || updatedDef.Revision != 2 || updatedDef.DisplayName != displayName {
		t.Fatalf("definition patch = %#v, %v", updatedDef, err)
	}
	if _, err := access.UpdateDefinition(ctx, createdDef.ID, createdDef.Revision, plugin.DefinitionPatch{DisplayName: &displayName}); !errors.Is(err, plugin.ErrConflict) {
		t.Fatalf("stale definition patch = %v, want conflict", err)
	}

	before, err := countRows(ctx, db, `SELECT count(*) FROM plugin_definition WHERE source = 'custom'`)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := access.CreateCustom(ctx,
		plugin.Definition{Namespace: "bad_endpoint", DisplayName: "Bad", Backend: plugin.BackendMCP, Spec: json.RawMessage(`{"endpoint":"https://secret.example"}`)},
		plugin.Config{Scope: plugin.ScopeUser, Enabled: boolPtr(false)}); !errors.Is(err, plugin.ErrInvalidDefinition) {
		t.Fatalf("endpoint in custom spec = %v, want invalid definition", err)
	}
	after, err := countRows(ctx, db, `SELECT count(*) FROM plugin_definition WHERE source = 'custom'`)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("rejected custom spec wrote a definition: before=%d after=%d", before, after)
	}

	if _, err := db.Exec(ctx, `
		INSERT INTO plugin_definition (id, namespace, display_name, backend, source, implementation_key, spec, default_enabled)
		VALUES ('builtin/taken', 'taken', 'Taken', 'go', 'builtin', 'builtin/taken', '{}'::jsonb, true)
	`); err != nil {
		t.Fatal(err)
	}
	value := true
	if _, err := access.CreateConfig(ctx, plugin.Config{PluginID: "builtin/taken", Scope: plugin.ScopeUser, Enabled: &value, Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := access.CreateCustom(ctx,
		plugin.Definition{Namespace: "taken", DisplayName: "Conflict", Backend: plugin.BackendMCP, Spec: json.RawMessage(`{}`)},
		plugin.Config{Scope: plugin.ScopeUser, Enabled: &value, Payload: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("namespace payload conflict was accepted")
	}
	var customCount int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_definition WHERE source = 'custom' AND namespace = 'taken'`).Scan(&customCount); err != nil {
		t.Fatal(err)
	}
	if customCount != 0 {
		t.Fatalf("failed custom create left definition rows: %d", customCount)
	}
}

func TestUnifiedPluginDefinitionDeleteCASAndPolicyRollback(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	user := insertPluginUser(t, db, "plugin-delete@example.test", false)
	service := plugin.NewService(db, nil, plugin.NewCatalog(), plugin.BackendPolicy{Validate: noopPluginValidator, Transition: noopBackendTransition}, inlinePluginMutationFence)
	access, err := service.Begin(user)
	if err != nil {
		t.Fatal(err)
	}
	def, config, err := access.CreateCustom(ctx,
		plugin.Definition{Namespace: "deletable", DisplayName: "Deletable", Backend: plugin.BackendMCP, Spec: json.RawMessage(`{}`)},
		plugin.Config{Scope: plugin.ScopeUser, Enabled: boolPtr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO tool_override (scope, user_id, enabled, plugin_id, local_tool_name)
		VALUES ('user', $1, true, $2, 'tool')
	`, user.UserID(), def.ID); err != nil {
		t.Fatal(err)
	}
	if err := access.DeleteDefinition(ctx, def.ID, def.Revision); err == nil {
		t.Fatal("definition with config was deleted")
	}
	var policies, definitions, configs int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM tool_override WHERE plugin_id = $1`, def.ID).Scan(&policies); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_definition WHERE id = $1`, def.ID).Scan(&definitions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_config WHERE id = $1`, config.ID).Scan(&configs); err != nil {
		t.Fatal(err)
	}
	if policies != 1 || definitions != 1 || configs != 1 {
		t.Fatalf("failed delete did not roll back: policies=%d definitions=%d configs=%d", policies, definitions, configs)
	}
	if err := access.DeleteConfig(ctx, def.ID, config.ID, config.Revision); err != nil {
		t.Fatal(err)
	}
	if err := access.DeleteDefinition(ctx, def.ID, def.Revision); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM tool_override WHERE plugin_id = $1`, def.ID).Scan(&policies); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_definition WHERE id = $1`, def.ID).Scan(&definitions); err != nil {
		t.Fatal(err)
	}
	if policies != 0 || definitions != 0 {
		t.Fatalf("successful delete left rows: policies=%d definitions=%d", policies, definitions)
	}
}

func countRows(ctx context.Context, db *pgxpool.Pool, query string) (int, error) {
	var count int
	if err := db.QueryRow(ctx, query).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func jsonEqual(left, right []byte) bool {
	var leftValue, rightValue any
	return json.Unmarshal(left, &leftValue) == nil && json.Unmarshal(right, &rightValue) == nil &&
		reflect.DeepEqual(leftValue, rightValue)
}

func boolPtr(value bool) *bool { return &value }
