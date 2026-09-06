package db

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CherryHQ/stella/internal/plugin"
)

func TestMCPConnectionStateReadsSharedAndTrustedUserOnly(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	userA := insertPluginUser(t, db, "mcp-state-a@example.test", false)
	userB := insertPluginUser(t, db, "mcp-state-b@example.test", false)
	definitionID := "builtin/mcp-connection-state-read"
	insertMCPConnectionStateDefinition(t, db, definitionID, "mcp-state-read")
	sharedConfigID := insertMCPConnectionStateConfig(t, db, definitionID, "mcp-state-read", string(plugin.ScopeSystem), "", 7)
	perUserConfigID := insertMCPConnectionStateConfig(t, db, definitionID, "mcp-state-read", string(plugin.ScopeUser), string(userA.UserID()), 3)
	foreignConfigID := insertMCPConnectionStateConfig(t, db, definitionID, "mcp-state-read", string(plugin.ScopeUser), string(userB.UserID()), 3)

	now := time.Now().UTC()
	storeMCPConnectionState(t, db, MCPConnectionState{
		ConfigID: sharedConfigID, Status: "ok", Tools: json.RawMessage(`[ {"name":"shared"} ]`), ConfigRevision: 7, ProbedAt: &now,
	})
	storeMCPConnectionState(t, db, MCPConnectionState{
		ConfigID: perUserConfigID, CredentialUserID: stringPtr(string(userA.UserID())), Status: "ok", Tools: json.RawMessage(`[{"name":"alpha"}]`), ConfigRevision: 3,
	})
	storeMCPConnectionState(t, db, MCPConnectionState{
		ConfigID: foreignConfigID, CredentialUserID: stringPtr(string(userB.UserID())), Status: "needs_auth", StatusError: "redacted", Tools: json.RawMessage(`[{"name":"beta"}]`), ConfigRevision: 3,
	})

	states, err := ListMCPConnectionStatesForConfigs(ctx, db, []string{sharedConfigID, perUserConfigID, foreignConfigID}, stringPtr(string(userA.UserID())))
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || states[0].ConfigID != sharedConfigID || states[1].ConfigID != perUserConfigID {
		t.Fatalf("trusted user states = %#v, want shared and user A only", states)
	}
	if states[1].CredentialUserID == nil || *states[1].CredentialUserID != string(userA.UserID()) {
		t.Fatalf("per-user owner = %#v", states[1].CredentialUserID)
	}

	states, err = ListMCPConnectionStatesForConfigs(ctx, db, []string{sharedConfigID, perUserConfigID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].ConfigID != sharedConfigID || states[0].CredentialUserID != nil {
		t.Fatalf("shared-only states = %#v", states)
	}
	if states, err = ListMCPConnectionStatesForConfigs(ctx, db, []string{foreignConfigID}, stringPtr(string(userA.UserID()))); err != nil {
		t.Fatal(err)
	} else if len(states) != 0 {
		t.Fatalf("foreign per-user state leaked: %#v", states)
	}
}

func TestMCPConnectionStateRevisionFenceAndCascade(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	user := insertPluginUser(t, db, "mcp-state-cascade@example.test", false)
	definitionID := "builtin/mcp-connection-state-write"
	insertMCPConnectionStateDefinition(t, db, definitionID, "mcp-state-write")
	configID := insertMCPConnectionStateConfig(t, db, definitionID, "mcp-state-write", string(plugin.ScopeSystem), "", 7)
	state := MCPConnectionState{ConfigID: configID, Status: "ok", Tools: json.RawMessage(`[]`), ConfigRevision: 7}
	storeMCPConnectionState(t, db, state)

	if _, err := db.Exec(ctx, `UPDATE plugin_config SET revision = 8, updated_at = now() WHERE id = $1`, configID); err != nil {
		t.Fatal(err)
	}
	if err := storeMCPConnectionStateErr(t, db, MCPConnectionState{ConfigID: configID, Status: "ok", Tools: json.RawMessage(`[]`), ConfigRevision: 7}); !errors.Is(err, ErrMCPConnectionStateStale) {
		t.Fatalf("stale replacement error = %v, want ErrMCPConnectionStateStale", err)
	}
	updated := MCPConnectionState{ConfigID: configID, Status: "ok", Tools: json.RawMessage(`[{"name":"fresh"}]`), ConfigRevision: 8}
	storeMCPConnectionState(t, db, updated)

	if _, err := db.Exec(ctx, `DELETE FROM plugin_config WHERE id = $1`, configID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM mcp_connection_state WHERE config_id = $1`, configID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("config cascade left %d observation rows", count)
	}

	userConfigID := insertMCPConnectionStateConfig(t, db, definitionID, "mcp-state-write", string(plugin.ScopeUser), string(user.UserID()), 1)
	storeMCPConnectionState(t, db, MCPConnectionState{ConfigID: userConfigID, CredentialUserID: stringPtr(string(user.UserID())), Status: "needs_auth", ConfigRevision: 1})
	if _, err := db.Exec(ctx, `DELETE FROM auth_user WHERE id = $1`, user.UserID()); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM mcp_connection_state WHERE config_id = $1`, userConfigID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("user cascade left %d observation rows", count)
	}
}

func TestMCPConnectionStateRejectsStaleFirstInsert(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	definitionID := "builtin/mcp-connection-state-empty"
	insertMCPConnectionStateDefinition(t, db, definitionID, "mcp-state-empty")
	configID := insertMCPConnectionStateConfig(t, db, definitionID, "mcp-state-empty", string(plugin.ScopeSystem), "", 8)
	if err := storeMCPConnectionStateErr(t, db, MCPConnectionState{ConfigID: configID, Status: "ok", ConfigRevision: 7}); !errors.Is(err, ErrMCPConnectionStateStale) {
		t.Fatalf("stale first insert error = %v, want ErrMCPConnectionStateStale", err)
	}
	if states, err := ListMCPConnectionStatesForConfigs(ctx, db, []string{configID}, nil); err != nil {
		t.Fatal(err)
	} else if len(states) != 0 {
		t.Fatalf("stale first insert created rows: %#v", states)
	}
}

func TestMCPConnectionStateRevisionFenceSerializesProbeAndConfigCAS(t *testing.T) {
	db := newTestDB(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	definitionID := "builtin/mcp-connection-state-race"
	insertMCPConnectionStateDefinition(t, db, definitionID, "mcp-state-race")
	configID := insertMCPConnectionStateConfig(t, db, definitionID, "mcp-state-race", string(plugin.ScopeSystem), "", 7)

	probeTx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = probeTx.Rollback(ctx) }()
	if _, err := StoreMCPConnectionState(ctx, probeTx, MCPConnectionState{ConfigID: configID, Status: "ok", ConfigRevision: 7}); err != nil {
		t.Fatal(err)
	}

	configTx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = configTx.Rollback(ctx) }()
	configDone := make(chan error, 1)
	go func() {
		_, updateErr := configTx.Exec(ctx, `UPDATE plugin_config SET revision = 8, updated_at = now() WHERE id = $1`, configID)
		if updateErr == nil {
			updateErr = configTx.Commit(ctx)
		}
		configDone <- updateErr
	}()

	if err := probeTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-configDone; err != nil {
		t.Fatal(err)
	}
	states, err := ListMCPConnectionStatesForConfigs(ctx, db, []string{configID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].ConfigRevision != 7 {
		t.Fatalf("race result = %#v, want persisted probe fenced at revision 7", states)
	}
	if err := storeMCPConnectionStateErr(t, db, MCPConnectionState{ConfigID: configID, Status: "ok", ConfigRevision: 7}); !errors.Is(err, ErrMCPConnectionStateStale) {
		t.Fatalf("post-CAS stale write error = %v, want ErrMCPConnectionStateStale", err)
	}
}

func insertMCPConnectionStateDefinition(t *testing.T, db *pgxpool.Pool, id, namespace string) {
	t.Helper()
	if _, err := db.Exec(t.Context(), `
		INSERT INTO plugin_definition (id, namespace, display_name, backend, source, implementation_key, spec, default_enabled, revision)
		VALUES ($1, $2, $1, 'mcp', 'builtin', 'mcp', '{}'::jsonb, false, 1)
	`, id, namespace); err != nil {
		t.Fatalf("insert definition %s: %v", id, err)
	}
}

func insertMCPConnectionStateConfig(t *testing.T, db *pgxpool.Pool, definitionID, namespace, scope, userID string, revision int64) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := db.Exec(t.Context(), `
		INSERT INTO plugin_config (id, plugin_id, namespace, scope, user_id, enabled, config, credential_refs, revision)
		VALUES ($1, $2, $3, $4, NULLIF($5, '')::uuid, true, '{}'::jsonb, '{}'::jsonb, $6)
	`, id, definitionID, namespace, scope, userID, revision); err != nil {
		t.Fatalf("insert config %s: %v", id, err)
	}
	return id
}

func storeMCPConnectionState(t *testing.T, db *pgxpool.Pool, state MCPConnectionState) {
	t.Helper()
	tx, err := db.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := StoreMCPConnectionState(t.Context(), tx, state); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func storeMCPConnectionStateErr(t *testing.T, db *pgxpool.Pool, state MCPConnectionState) error {
	t.Helper()
	tx, err := db.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	_, err = StoreMCPConnectionState(t.Context(), tx, state)
	return err
}

func stringPtr(value string) *string {
	return &value
}
