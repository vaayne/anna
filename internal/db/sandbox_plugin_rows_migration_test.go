package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	dropSandboxPluginRowsBeforeMigration = 20260801061950
	dropSandboxPluginRowsMigration       = 20260804120000
)

// TestDropSandboxPluginRows pins the blast radius of the delete: every
// sandbox/<name> row goes, and nothing else is touched. The seeded ids include
// the near misses that a careless LIKE pattern would swallow ("sandbox" with no
// slash, "sandboxed/local", a tool whose name merely contains "sandbox").
func TestDropSandboxPluginRows(t *testing.T) {
	db, provider := newTestDBAtMigration(t, dropSandboxPluginRowsBeforeMigration)
	ctx := t.Context()

	deleted := []string{"sandbox/docker", "sandbox/local", "sandbox/none", "sandbox/custom"}
	kept := []string{
		"tool/gh",
		"channel/telegram",
		"hook/rtk",
		"provider/openai",
		"sandbox",          // no slash: not a sandbox plugin id
		"sandboxed/local",  // "/" in the pattern is a literal, not a wildcard
		"tool/sandbox-fmt", // "sandbox" outside the kind segment
	}
	for _, id := range append(append([]string{}, deleted...), kept...) {
		seedPluginRow(t, db, id)
	}

	if _, err := provider.UpTo(ctx, dropSandboxPluginRowsMigration); err != nil {
		t.Fatalf("run sandbox plugin row cleanup: %v", err)
	}

	for _, id := range deleted {
		if pluginRowExists(t, db, id) {
			t.Errorf("plugin row %q survived the cleanup", id)
		}
		if pluginStateExists(t, db, id) {
			t.Errorf("plugin_state row %q survived the cleanup", id)
		}
	}
	for _, id := range kept {
		if !pluginRowExists(t, db, id) {
			t.Errorf("plugin row %q was deleted; the cleanup is too broad", id)
		}
		if !pluginStateExists(t, db, id) {
			t.Errorf("plugin_state row %q was deleted; the cleanup is too broad", id)
		}
	}

	// Re-applying an already-applied migration is the supported idempotence
	// check. The current database may not be rolled back through irreversible
	// migration 41 just to reach this historical boundary.
	if _, err := provider.UpTo(ctx, dropSandboxPluginRowsMigration); err != nil {
		t.Fatalf("repeat cleanup: %v", err)
	}
	for _, id := range kept {
		if !pluginRowExists(t, db, id) {
			t.Errorf("plugin row %q lost on the second run", id)
		}
	}
}

func seedPluginRow(t *testing.T, db *pgxpool.Pool, id string) {
	t.Helper()
	kind, name := id, id
	for i := range id {
		if id[i] == '/' {
			kind, name = id[:i], id[i+1:]
			break
		}
	}
	if _, err := db.Exec(context.Background(), `
		INSERT INTO plugin (id, kind, name, enabled) VALUES ($1, $2, $3, true)
	`, id, kind, name); err != nil {
		t.Fatalf("seed plugin %q: %v", id, err)
	}
	if _, err := db.Exec(context.Background(), `
		INSERT INTO plugin_state (plugin_id, scope_kind, scope_id, state_key, value)
		VALUES ($1, 'global', '', 'probe', '{}'::jsonb)
	`, id); err != nil {
		t.Fatalf("seed plugin_state %q: %v", id, err)
	}
}

func pluginRowExists(t *testing.T, db *pgxpool.Pool, id string) bool {
	t.Helper()
	var count int
	if err := db.QueryRow(context.Background(),
		`SELECT count(*) FROM plugin WHERE id = $1`, id).Scan(&count); err != nil {
		t.Fatalf("count plugin %q: %v", id, err)
	}
	return count > 0
}

func pluginStateExists(t *testing.T, db *pgxpool.Pool, id string) bool {
	t.Helper()
	var count int
	if err := db.QueryRow(context.Background(),
		`SELECT count(*) FROM plugin_state WHERE plugin_id = $1`, id).Scan(&count); err != nil {
		t.Fatalf("count plugin_state %q: %v", id, err)
	}
	return count > 0
}
