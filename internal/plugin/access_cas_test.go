package plugin

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CherryHQ/stella/internal/authz"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	"github.com/CherryHQ/stella/internal/db/dbtest"
	"github.com/CherryHQ/stella/internal/platform/config"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

func TestAccessUpdateConfigUsesExpectedRevision(t *testing.T) {
	db := dbtest.New(t)
	catalog := NewCatalog()
	definition := Definition{
		ID: "tool/cas", Namespace: "cas", DisplayName: "CAS",
		Backend: BackendGo, Source: SourceBuiltin, ImplementationKey: "tool/cas",
		Spec: []byte(`{}`), DefaultEnabled: true, Revision: 1,
	}
	if err := catalog.Register(definition); err != nil {
		t.Fatal(err)
	}
	service := NewService(db, nil, catalog, BackendPolicy{Transition: inlineBackendPolicyTransition}, func(_ context.Context, fn func() error) error { return fn() })
	if err := service.SyncBuiltinDefaults(t.Context()); err != nil {
		t.Fatalf("sync defaults: %v", err)
	}
	authority, err := authz.NewUserAuthority("10000000-0000-0000-0000-000000000001", true)
	if err != nil {
		t.Fatal(err)
	}
	access, err := service.Begin(authority)
	if err != nil {
		t.Fatal(err)
	}
	configs, err := access.ListConfigs(t.Context(), definition.ID, ScopeSystem, "")
	if err != nil {
		t.Fatalf("list system configs: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("system config count = %d, want 1", len(configs))
	}
	initialRevision := configs[0].Revision
	disabled := false
	updated, err := access.UpdateConfig(t.Context(), definition.ID, configs[0].ID, initialRevision, ConfigPatch{
		EnabledSet: true,
		Enabled:    &disabled,
	})
	if err != nil {
		t.Fatalf("disable config: %v", err)
	}
	if updated.Revision <= initialRevision || updated.Enabled == nil || *updated.Enabled {
		t.Fatalf("updated config = %+v, want disabled revision", updated)
	}
	if _, err := access.UpdateConfig(t.Context(), definition.ID, configs[0].ID, initialRevision, ConfigPatch{
		EnabledSet: true,
		Enabled:    &disabled,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update error = %v, want ErrConflict", err)
	}
}

func TestAccessMoveConfigPreservesIDAndDerivesTargetUser(t *testing.T) {
	db := dbtest.New(t)
	userID := seedMoveUser(t, db)
	seedMoveAgent(t, db, "agent")
	service, definition := newMoveService(t, db, agentaccess.NewService(assignedAgentStore{}, assignedAgentLinks{}), nil)
	authority, err := authz.NewUserAuthority(authz.UserID(userID), false)
	if err != nil {
		t.Fatal(err)
	}
	access, err := service.Begin(authority)
	if err != nil {
		t.Fatal(err)
	}
	created, err := access.CreateConfig(t.Context(), Config{PluginID: definition.ID, Scope: ScopeUser, Enabled: boolPtr(false)})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}

	moved, err := access.MoveConfig(t.Context(), definition.ID, created.ID, created.Revision, ScopeUserAgent, "agent", ConfigPatch{})
	if err != nil {
		t.Fatalf("MoveConfig: %v", err)
	}
	if moved.ID != created.ID || moved.Revision != created.Revision+1 {
		t.Fatalf("moved identity = %s/revision %d, want %s/revision %d", moved.ID, moved.Revision, created.ID, created.Revision+1)
	}
	if moved.Scope != ScopeUserAgent || moved.UserID != userID || moved.AgentID != "agent" {
		t.Fatalf("moved owner = %q/%q/%q, want user_agent/%s/agent", moved.Scope, moved.UserID, moved.AgentID, userID)
	}
}

func TestAccessMoveConfigRejectsUnauthorizedAgentSource(t *testing.T) {
	db := dbtest.New(t)
	userID := seedMoveUser(t, db)
	seedMoveAgent(t, db, "agent")
	allowedService, definition := newMoveService(t, db, agentaccess.NewService(assignedAgentStore{}, assignedAgentLinks{}), nil)
	authority, err := authz.NewUserAuthority(authz.UserID(userID), false)
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := allowedService.Begin(authority)
	if err != nil {
		t.Fatal(err)
	}
	created, err := allowed.CreateConfig(t.Context(), Config{PluginID: definition.ID, Scope: ScopeUserAgent, AgentID: "agent", Enabled: boolPtr(false)})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}

	deniedService := NewService(db, agentaccess.NewService(denyingMoveAgentStore{}, denyingMoveAgentLinks{}), allowedService.catalog, BackendPolicy{Transition: inlineBackendPolicyTransition}, func(_ context.Context, fn func() error) error { return fn() })
	denied, err := deniedService.Begin(authority)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := denied.MoveConfig(t.Context(), definition.ID, created.ID, created.Revision, ScopeUser, "", ConfigPatch{}); err == nil {
		t.Fatal("MoveConfig accepted an unauthorized source agent")
	}
}

func TestAccessMoveConfigRejectsBuiltinSystemSource(t *testing.T) {
	db := dbtest.New(t)
	service, definition := newMoveService(t, db, nil, nil)
	authority, err := authz.NewUserAuthority(authz.UserID("10000000-0000-0000-0000-000000000001"), true)
	if err != nil {
		t.Fatal(err)
	}
	access, err := service.Begin(authority)
	if err != nil {
		t.Fatal(err)
	}
	configs, err := access.ListConfigs(t.Context(), definition.ID, ScopeSystem, "")
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs = %d/%v, want one system config", len(configs), err)
	}
	if _, err := access.MoveConfig(t.Context(), definition.ID, configs[0].ID, configs[0].Revision, ScopeUser, "", ConfigPatch{}); !errors.Is(err, ErrBuiltinConfig) {
		t.Fatalf("MoveConfig error = %v, want ErrBuiltinConfig", err)
	}
}

func TestAccessMoveConfigRejectsStaleCASAndTargetCollision(t *testing.T) {
	db := dbtest.New(t)
	userID := seedMoveUser(t, db)
	seedMoveAgent(t, db, "agent")
	service, definition := newMoveService(t, db, agentaccess.NewService(assignedAgentStore{}, assignedAgentLinks{}), nil)
	authority, err := authz.NewUserAuthority(authz.UserID(userID), false)
	if err != nil {
		t.Fatal(err)
	}
	access, err := service.Begin(authority)
	if err != nil {
		t.Fatal(err)
	}
	first, err := access.CreateConfig(t.Context(), Config{PluginID: definition.ID, Scope: ScopeUser, Enabled: boolPtr(false)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := access.CreateConfig(t.Context(), Config{PluginID: definition.ID, Scope: ScopeUserAgent, AgentID: "agent", Enabled: boolPtr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := access.MoveConfig(t.Context(), definition.ID, first.ID, first.Revision, ScopeUserAgent, "agent", ConfigPatch{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("target collision error = %v, want ErrConflict", err)
	}
	unchanged, err := access.GetConfig(t.Context(), definition.ID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Scope != ScopeUser || unchanged.Revision != first.Revision {
		t.Fatalf("collision mutated source = %+v", unchanged)
	}
	moved, err := access.MoveConfig(t.Context(), definition.ID, first.ID, first.Revision, ScopeUser, "", ConfigPatch{})
	if err != nil {
		t.Fatalf("same-tuple move: %v", err)
	}
	if _, err := access.MoveConfig(t.Context(), definition.ID, first.ID, first.Revision, ScopeUser, "", ConfigPatch{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale CAS error = %v, want ErrConflict", err)
	}
	if moved.ID != first.ID || moved.Revision != first.Revision+1 {
		t.Fatalf("same-tuple move = %+v", moved)
	}
	_ = second
}

func TestAccessMoveConfigValidatesBeforeMutation(t *testing.T) {
	db := dbtest.New(t)
	userID := seedMoveUser(t, db)
	service, definition := newMoveService(t, db, nil, func(context.Context, Definition, Config, []string) error { return nil })
	authority, err := authz.NewUserAuthority(authz.UserID(userID), false)
	if err != nil {
		t.Fatal(err)
	}
	access, err := service.Begin(authority)
	if err != nil {
		t.Fatal(err)
	}
	created, err := access.CreateConfig(t.Context(), Config{PluginID: definition.ID, Scope: ScopeUser, Enabled: boolPtr(false), Payload: []byte(`{"version":"1"}`)})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	service.policy.Validate = func(context.Context, Definition, Config, []string) error { return ErrInvalidConfig }
	if _, err := access.MoveConfig(t.Context(), definition.ID, created.ID, created.Revision, ScopeUser, "", ConfigPatch{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("validation error = %v, want ErrInvalidConfig", err)
	}
	unchanged, err := access.GetConfig(t.Context(), definition.ID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Scope != ScopeUser || unchanged.Revision != created.Revision {
		t.Fatalf("failed move mutated config = %+v", unchanged)
	}
}

func newMoveService(t *testing.T, db *pgxpool.Pool, agents *agentaccess.Service, validate PayloadValidator) (*Service, Definition) {
	t.Helper()
	catalog := NewCatalog()
	definition := Definition{ID: "tool/move", Namespace: "move", DisplayName: "Move", Backend: BackendGo, Source: SourceBuiltin, ImplementationKey: "tool/move", Spec: []byte(`{}`), DefaultEnabled: false, Revision: 1}
	if err := catalog.Register(definition); err != nil {
		t.Fatal(err)
	}
	service := NewService(db, agents, catalog, BackendPolicy{Validate: validate, Transition: inlineBackendPolicyTransition}, func(_ context.Context, fn func() error) error { return fn() })
	if err := service.SyncBuiltinDefaults(t.Context()); err != nil {
		t.Fatalf("SyncBuiltinDefaults: %v", err)
	}
	return service, definition
}

func seedMoveUser(t *testing.T, db *pgxpool.Pool) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := db.Exec(t.Context(), `INSERT INTO auth_user (id, email) VALUES ($1, $2)`, id, id+"@move.test"); err != nil {
		t.Fatalf("insert move user: %v", err)
	}
	return id
}

func seedMoveAgent(t *testing.T, db *pgxpool.Pool, id string) {
	t.Helper()
	if _, err := db.Exec(t.Context(), `INSERT INTO agent (id, name, workspace, scope, creator_id) VALUES ($1, $2, '/tmp', 'restricted', '')`, id, id); err != nil {
		t.Fatalf("insert move agent: %v", err)
	}
}

type denyingMoveAgentStore struct{}

func (denyingMoveAgentStore) GetAgent(context.Context, string) (config.Agent, error) {
	return config.Agent{ID: "agent", Scope: config.AgentScopeRestricted, Enabled: true}, nil
}

func (denyingMoveAgentStore) ListAgents(context.Context) ([]config.Agent, error) { return nil, nil }

type denyingMoveAgentLinks struct{}

func (denyingMoveAgentLinks) ListUserAgentIDs(context.Context, string) ([]string, error) {
	return nil, nil
}

func TestBackendPolicyCASConflictSkipsTransition(t *testing.T) {
	db := dbtest.New(t)
	catalog := NewCatalog()
	definition := backendPolicyDefinition("policy-cas", false)
	if err := catalog.Register(definition); err != nil {
		t.Fatal(err)
	}
	var transitions int
	service := NewService(db, nil, catalog, BackendPolicy{
		Transition: func(context.Context, pgx.Tx, authz.Authority, MutationKind, Definition, *Config, *Config) error {
			transitions++
			return nil
		},
	}, inlineBackendPolicyFence)
	if err := service.SyncBuiltinDefaults(t.Context()); err != nil {
		t.Fatalf("sync defaults: %v", err)
	}
	authority, err := authz.NewUserAuthority("10000000-0000-0000-0000-000000000001", true)
	if err != nil {
		t.Fatal(err)
	}
	access, err := service.Begin(authority)
	if err != nil {
		t.Fatal(err)
	}
	configs, err := access.ListConfigs(t.Context(), definition.ID, ScopeSystem, "")
	if err != nil || len(configs) != 1 {
		t.Fatalf("list system config = %d/%v, want one", len(configs), err)
	}
	initial := configs[0]
	disabled := false
	if _, err := access.UpdateConfig(t.Context(), definition.ID, initial.ID, initial.Revision, ConfigPatch{EnabledSet: true, Enabled: &disabled}); err != nil {
		t.Fatalf("first update: %v", err)
	}
	if transitions != 1 {
		t.Fatalf("transition count after first update = %d, want 1", transitions)
	}
	if _, err := access.UpdateConfig(t.Context(), definition.ID, initial.ID, initial.Revision, ConfigPatch{EnabledSet: true, Enabled: &disabled}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update = %v, want ErrConflict", err)
	}
	if transitions != 1 {
		t.Fatalf("transition count after stale update = %d, want unchanged", transitions)
	}
}

func TestBackendPolicyTransitionErrorRollsBackConfig(t *testing.T) {
	db := dbtest.New(t)
	catalog := NewCatalog()
	definition := backendPolicyDefinition("policy-rollback", true)
	if err := catalog.Register(definition); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("backend transition rejected")
	var hookSawCommittedRow bool
	service := NewService(db, nil, catalog, BackendPolicy{
		Transition: func(ctx context.Context, tx pgx.Tx, _ authz.Authority, _ MutationKind, _ Definition, _ *Config, after *Config) error {
			var revision int64
			if err := tx.QueryRow(ctx, `SELECT revision FROM plugin_config WHERE id = $1`, after.ID).Scan(&revision); err != nil {
				return err
			}
			hookSawCommittedRow = revision == after.Revision
			return wantErr
		},
	}, inlineBackendPolicyFence)
	if err := service.SyncBuiltinDefaults(t.Context()); err != nil {
		t.Fatalf("sync defaults: %v", err)
	}
	authority, err := authz.NewUserAuthority("10000000-0000-0000-0000-000000000001", true)
	if err != nil {
		t.Fatal(err)
	}
	access, err := service.Begin(authority)
	if err != nil {
		t.Fatal(err)
	}
	configs, err := access.ListConfigs(t.Context(), definition.ID, ScopeSystem, "")
	if err != nil || len(configs) != 1 {
		t.Fatalf("list system config = %d/%v, want one", len(configs), err)
	}
	before := configs[0]
	disabled := false
	if _, err := access.UpdateConfig(t.Context(), definition.ID, before.ID, before.Revision, ConfigPatch{EnabledSet: true, Enabled: &disabled}); !errors.Is(err, wantErr) {
		t.Fatalf("transition error = %v, want %v", err, wantErr)
	}
	if !hookSawCommittedRow {
		t.Fatal("transition did not observe the post-CAS row in the same transaction")
	}
	after, err := access.GetConfig(t.Context(), definition.ID, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || !sameBackendPolicyBool(after.Enabled, before.Enabled) {
		t.Fatalf("config after transition rollback = %+v, want revision/enabled %+v", after, before)
	}
}

func TestBackendPolicyValidateOnlyFailsClosedAndRollsBack(t *testing.T) {
	db := dbtest.New(t)
	catalog := NewCatalog()
	definition := backendPolicyDefinition("policy-no-transition", false)
	if err := catalog.Register(definition); err != nil {
		t.Fatal(err)
	}
	service := NewService(db, nil, catalog, BackendPolicy{
		Validate: func(context.Context, Definition, Config, []string) error { return nil },
	}, inlineBackendPolicyFence)
	if err := service.SyncBuiltinDefaults(t.Context()); err != nil {
		t.Fatalf("sync defaults: %v", err)
	}
	authority, err := authz.NewUserAuthority("10000000-0000-0000-0000-000000000001", true)
	if err != nil {
		t.Fatal(err)
	}
	access, err := service.Begin(authority)
	if err != nil {
		t.Fatal(err)
	}
	configs, err := access.ListConfigs(t.Context(), definition.ID, ScopeSystem, "")
	if err != nil || len(configs) != 1 {
		t.Fatalf("list system config = %d/%v, want one", len(configs), err)
	}
	before := configs[0]
	disabled := false
	if _, err := access.UpdateConfig(t.Context(), definition.ID, before.ID, before.Revision, ConfigPatch{EnabledSet: true, Enabled: &disabled}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("validate-only update = %v, want ErrInvalidConfig", err)
	}
	after, err := access.GetConfig(t.Context(), definition.ID, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || !sameBackendPolicyBool(after.Enabled, before.Enabled) {
		t.Fatalf("config after fail-closed rollback = %+v, want unchanged %+v", after, before)
	}
}

func sameBackendPolicyBool(left, right *bool) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func backendPolicyDefinition(id string, defaultEnabled bool) Definition {
	return Definition{
		ID: id, Namespace: id, DisplayName: id, Backend: BackendGo, Source: SourceBuiltin,
		ImplementationKey: id, Spec: []byte(`{}`), DefaultEnabled: defaultEnabled, Revision: 1,
	}
}

func inlineBackendPolicyFence(_ context.Context, fn func() error) error { return fn() }

func inlineBackendPolicyTransition(context.Context, pgx.Tx, authz.Authority, MutationKind, Definition, *Config, *Config) error {
	return nil
}
