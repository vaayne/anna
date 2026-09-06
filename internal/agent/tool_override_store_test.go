package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CherryHQ/stella/internal/auth"
	appdb "github.com/CherryHQ/stella/internal/db"
	"github.com/CherryHQ/stella/internal/db/dbtest"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/pkg/db/pgnull"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

func TestToolOverrideStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := dbtest.New(t)
	q := sqlc.New(db)
	oidc := appdb.NewOIDCStore(db)

	user, err := oidc.CreateUser(ctx, auth.User{ID: uuid.NewString(), Email: "tools@test.local", Name: "Tools"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agentID := "tool-override-agent"
	if _, err := q.CreateAgent(ctx, sqlc.CreateAgentParams{
		ID: agentID, Name: "Tools Agent", Workspace: "/tmp/tools-agent",
		Sandbox: json.RawMessage(`{}`),
		Scope:   "system", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	row, err := q.UpsertCoreToolOverride(ctx, sqlc.UpsertCoreToolOverrideParams{
		ToolName: pgnull.Text("memory"), Scope: ToolOverrideScopeUserAgent,
		UserID: pgnull.Text(user.ID), AgentID: pgnull.Text(agentID), Enabled: false,
	})
	if err != nil {
		t.Fatalf("UpsertCoreToolOverride: %v", err)
	}
	if !row.ToolName.Valid || row.ToolName.String != "memory" || row.Enabled {
		t.Fatalf("row = %+v, want disabled memory", row)
	}

	rows, err := q.ListToolOverridesForAgentContext(ctx, sqlc.ListToolOverridesForAgentContextParams{
		UserID: pgnull.Text(user.ID), AgentID: pgnull.Text(agentID),
	})
	if err != nil {
		t.Fatalf("ListToolOverridesForAgentContext: %v", err)
	}
	if len(rows) != 1 || !rows[0].ToolName.Valid || rows[0].ToolName.String != "memory" || rows[0].Scope != ToolOverrideScopeUserAgent {
		t.Fatalf("rows = %+v, want one memory user_agent override", rows)
	}

	// ToolOverrideStore.Fetch must expose the same row through the
	// ToolOverrideFetcher signature.
	var fetch ToolOverrideFetcher = NewToolOverrideStore(db).Fetch
	fetched, err := fetch(ctx, user.ID, agentID)
	if err != nil {
		t.Fatalf("ToolOverrideStore.Fetch: %v", err)
	}
	if len(fetched) != 1 || fetched[0] != (ToolOverride{Identity: ToolIdentity{CoreToolName: "memory"}, Scope: ToolOverrideScopeUserAgent, Enabled: false}) {
		t.Fatalf("fetched = %+v, want one disabled memory user_agent override", fetched)
	}
	versions, err := NewToolOverrideStore(db).ListVersions(ctx, user.ID, agentID)
	if err != nil {
		t.Fatalf("ToolOverrideStore.ListVersions: %v", err)
	}
	version, ok := versions["memory"]
	if len(versions) != 1 || !ok || !version.Present || version.Version == ToolOverrideAbsentVersion {
		t.Fatalf("versions = %+v, want one present versioned memory override", versions)
	}

	if err := q.DeleteCoreToolOverride(ctx, sqlc.DeleteCoreToolOverrideParams{
		ToolName: pgnull.Text("memory"), Scope: ToolOverrideScopeUserAgent,
		UserID: pgnull.Text(user.ID), AgentID: pgnull.Text(agentID),
	}); err != nil {
		t.Fatalf("DeleteCoreToolOverride: %v", err)
	}
	rows, err = q.ListToolOverridesForAgentContext(ctx, sqlc.ListToolOverridesForAgentContextParams{
		UserID: pgnull.Text(user.ID), AgentID: pgnull.Text(agentID),
	})
	if err != nil {
		t.Fatalf("ListToolOverridesForAgentContext after delete: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows after delete = %+v, want none", rows)
	}
}

func TestToolOverrideStoreConditionalWrites(t *testing.T) {
	ctx := context.Background()
	db := dbtest.New(t)
	store := NewToolOverrideStore(db)
	key := ToolOverrideKey{Identity: ToolIdentity{CoreToolName: "scheduler_job_list"}, Scope: ToolOverrideScopeSystem}

	absent, err := store.Get(ctx, key)
	if err != nil || absent.Version != ToolOverrideAbsentVersion || absent.Present {
		t.Fatalf("absent Get = %+v, %v", absent, err)
	}
	created, err := store.SetIfVersion(ctx, ToolOverrideWrite{Identity: key.Identity, Scope: key.Scope, UserID: key.UserID, AgentID: key.AgentID, Enabled: false}, absent.Version)
	if err != nil || !created.Present || created.Version == ToolOverrideAbsentVersion {
		t.Fatalf("absent SetIfVersion = %+v, %v", created, err)
	}
	if _, err := store.SetIfVersion(ctx, ToolOverrideWrite{Identity: key.Identity, Scope: key.Scope, UserID: key.UserID, AgentID: key.AgentID, Enabled: true}, ToolOverrideAbsentVersion); !errors.Is(err, config.ErrAgentVersionConflict) {
		t.Fatalf("concurrent absent insert error = %v, want conflict", err)
	}
	updated, err := store.SetIfVersion(ctx, ToolOverrideWrite{Identity: key.Identity, Scope: key.Scope, UserID: key.UserID, AgentID: key.AgentID, Enabled: true}, created.Version)
	if err != nil || !updated.Enabled || updated.Version == created.Version {
		t.Fatalf("existing SetIfVersion = %+v, %v", updated, err)
	}
	if err := store.ClearIfVersion(ctx, key, created.Version); !errors.Is(err, config.ErrAgentVersionConflict) {
		t.Fatalf("stale ClearIfVersion error = %v, want conflict", err)
	}
	if err := store.ClearIfVersion(ctx, key, updated.Version); err != nil {
		t.Fatalf("fresh ClearIfVersion: %v", err)
	}
}

// TestToolOverrideStoreRejectsUnknownScope proves the Agent domain owns the
// scope-vocabulary invariant on write: an unrecognized scope fails before any
// query runs, so the transport can never persist an override under a bogus scope.
func TestToolOverrideStoreRejectsUnknownScope(t *testing.T) {
	s := &ToolOverrideStore{}
	if err := s.Set(context.Background(), ToolOverrideWrite{Identity: ToolIdentity{CoreToolName: "memory"}, Scope: "bogus", Enabled: true}); err == nil {
		t.Fatal("Set with unknown scope = nil, want error")
	}
	if err := s.Clear(context.Background(), ToolOverrideKey{Identity: ToolIdentity{CoreToolName: "memory"}, Scope: "bogus"}); err == nil {
		t.Fatal("Clear with unknown scope = nil, want error")
	}
}

func TestToolOverrideStorePluginIdentityCRUDAndCAS(t *testing.T) {
	ctx := context.Background()
	db := dbtest.New(t)
	applyToolOverrideIdentityDDL(t, db)
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin_definition (id, namespace, display_name, backend, source, implementation_key, spec, default_enabled, revision)
		VALUES ('system/email', 'email', 'Email', 'go', 'builtin', 'email', '{}'::jsonb, true, 1)
	`); err != nil {
		t.Fatalf("insert plugin definition: %v", err)
	}

	identity := ToolIdentity{PluginID: "system/email", LocalToolName: "message_send"}
	key := ToolOverrideKey{Identity: identity, Scope: ToolOverrideScopeSystem}
	store := NewToolOverrideStore(db)
	absent, err := store.Get(ctx, key)
	if err != nil || absent.Present || absent.Version != ToolOverrideAbsentVersion || absent.Identity == nil || *absent.Identity != identity {
		t.Fatalf("plugin absent Get = %+v, %v", absent, err)
	}
	created, err := store.SetIfVersion(ctx, ToolOverrideWrite{Identity: identity, Scope: key.Scope, Enabled: false}, absent.Version)
	if err != nil || !created.Present || created.Enabled || created.Identity == nil || *created.Identity != identity {
		t.Fatalf("plugin create = %+v, %v", created, err)
	}
	fetched, err := store.Fetch(ctx, "", "")
	if err != nil || len(fetched) != 1 || fetched[0].Identity != identity || fetched[0].Enabled {
		t.Fatalf("plugin Fetch = %+v, %v", fetched, err)
	}
	updated, err := store.SetIfVersion(ctx, ToolOverrideWrite{Identity: identity, Scope: key.Scope, Enabled: true}, created.Version)
	if err != nil || !updated.Enabled || updated.Identity == nil || *updated.Identity != identity {
		t.Fatalf("plugin update = %+v, %v", updated, err)
	}
	if err := store.ClearIfVersion(ctx, key, created.Version); !errors.Is(err, config.ErrAgentVersionConflict) {
		t.Fatalf("stale plugin clear = %v, want conflict", err)
	}
	if err := store.ClearIfVersion(ctx, key, updated.Version); err != nil {
		t.Fatalf("fresh plugin clear: %v", err)
	}
}

func applyToolOverrideIdentityDDL(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	_, err := db.Exec(context.Background(), `
		ALTER TABLE tool_override ALTER COLUMN tool_name DROP NOT NULL;
		ALTER TABLE tool_override DROP CONSTRAINT IF EXISTS tool_override_tool_name_scope_user_id_agent_id_key;
		CREATE UNIQUE INDEX IF NOT EXISTS uniq_tool_override_core_identity
			ON tool_override (tool_name, scope, user_id, agent_id) NULLS NOT DISTINCT
			WHERE tool_name IS NOT NULL AND plugin_id IS NULL AND local_tool_name IS NULL;
		CREATE UNIQUE INDEX IF NOT EXISTS uniq_tool_override_plugin_identity
			ON tool_override (plugin_id, local_tool_name, scope, user_id, agent_id) NULLS NOT DISTINCT
			WHERE tool_name IS NULL;
	`)
	if err != nil {
		t.Fatalf("apply tool override identity DDL: %v", err)
	}
}

func TestToolOverrideStoreRejectsMalformedIdentity(t *testing.T) {
	s := &ToolOverrideStore{}
	_, err := s.Get(context.Background(), ToolOverrideKey{
		Identity: ToolIdentity{PluginID: "system/email"},
		Scope:    ToolOverrideScopeSystem,
	})
	if err == nil {
		t.Fatal("Get malformed identity = nil, want validation error")
	}
}

func TestPersistedToolIdentityUsesStoredPluginPair(t *testing.T) {
	row := sqlc.ToolOverride{
		ToolName:      pgnull.Text("email__send"),
		PluginID:      pgnull.Text("system/email"),
		LocalToolName: pgnull.Text("send"),
	}
	identity, err := persistedToolIdentity(row)
	if err != nil {
		t.Fatalf("persistedToolIdentity: %v", err)
	}
	if identity != (ToolIdentity{PluginID: "system/email", LocalToolName: "send"}) {
		t.Fatalf("identity = %+v, want trusted plugin/local pair", identity)
	}
}

func TestPersistedToolIdentityRejectsPartialPluginPair(t *testing.T) {
	row := sqlc.ToolOverride{PluginID: pgnull.Text("system/email")}
	if _, err := persistedToolIdentity(row); err == nil {
		t.Fatal("partial persisted plugin identity = nil, want error")
	}
}
