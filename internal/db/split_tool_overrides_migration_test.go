package db

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	splitToolOverridesBeforeMigration = 90000000000025
	splitToolOverridesMigration       = 90000000000026
)

// retiredToolNames are the names this change removes: the seven union tools and
// the four recally tools it renamed. The pre-#1171 "recally" union is not here,
// because #1171 retired it, not this change.
var retiredToolNames = []string{
	"goal", "scheduler", "workflow", "vault", "oauth", "share", "email",
	"recally_get_article", "recally_list_articles", "recally_save_article", "recally_digest",
}

// A tool_override row is keyed by name, so a row naming a retired tool matches
// nothing after the split: it hides neither the old capability nor any of the
// new ones, and it waits there for a future tool to reuse the name and inherit
// the setting. Stella is pre-production, so the rows go rather than being
// migrated forward — the capability falls back to its default visibility.
//
// The assertion that matters is the second one: deleting by name must not reach
// a name this change did not retire.
func TestSplitToolOverridesDeletesOnlyTheRetiredNames(t *testing.T) {
	db, provider := newTestDBAtMigration(t, splitToolOverridesBeforeMigration)
	ctx := t.Context()
	for _, name := range retiredToolNames {
		seedToolOverride(t, db, name, false)
	}
	// Two survivors: a name this change never touched, and one of the new names
	// an operator could already have written a row for.
	seedToolOverride(t, db, "memory", false)
	seedToolOverride(t, db, "goal_list", true)

	if _, err := provider.UpTo(ctx, splitToolOverridesMigration); err != nil {
		t.Fatalf("migrate tool overrides: %v", err)
	}

	assertExactSystemOverrides(t, db, map[string]bool{"memory": false, "goal_list": true})
}

const (
	settingsToolRenameBeforeMigration = 90000000000031
	settingsToolRenameMigration       = 90000000000032
)

var settingsToolNamesRetiredByRename = []string{
	"agent_list", "agent_get", "agent_create", "agent_update", "agent_delete",
	"agent_tool_list", "agent_tool_update", "agent_tool_delete",
	"library_file_list", "library_file_get", "library_file_upload", "library_file_delete",
	"skill_list", "skill_get", "skill_create", "skill_update", "skill_delete",
	"provider_list", "provider_get", "provider_create", "provider_update", "provider_delete",
	"default_model_get", "default_model_update",
	"embedding_setting_get", "embedding_setting_update",
	"plugin_list", "plugin_enable", "plugin_disable",
	"mcp_server_list", "mcp_server_get", "mcp_server_create", "mcp_server_update", "mcp_server_delete",
}

func TestSettingsToolRenameDeletesOnlyRetiredNames(t *testing.T) {
	db, provider := newTestDBAtMigration(t, settingsToolRenameBeforeMigration)
	ctx := t.Context()
	if got := len(settingsToolNamesRetiredByRename); got != 34 {
		t.Fatalf("retired Settings tool inventory = %d, want 34", got)
	}
	for _, name := range settingsToolNamesRetiredByRename {
		seedToolOverride(t, db, name, false)
	}
	seedToolOverride(t, db, "memory", false)
	seedToolOverride(t, db, "settings_agent_list", true)

	const userID = "00000000-0000-0000-0000-000000000032"
	const agentID = "settings-override-migration"
	if _, err := db.Exec(ctx, `INSERT INTO auth_user (id, email) VALUES ($1, 'settings-override-migration@test.invalid')`, userID); err != nil {
		t.Fatalf("seed override user: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO agent (id, name, workspace) VALUES ($1, $1, '/tmp/settings-override-migration')`, agentID); err != nil {
		t.Fatalf("seed override Agent: %v", err)
	}
	seedScopedToolOverride(t, db, "provider_list", "system_agent", nil, agentID, false)
	seedScopedToolOverride(t, db, "plugin_list", "user", userID, nil, false)
	seedScopedToolOverride(t, db, "mcp_server_list", "user_agent", userID, agentID, false)
	seedScopedToolOverride(t, db, "settings_provider_list", "system_agent", nil, agentID, true)
	seedScopedToolOverride(t, db, "memory", "user", userID, nil, true)
	seedScopedToolOverride(t, db, "settings_mcp_server_list", "user_agent", userID, agentID, true)

	if _, err := provider.UpTo(ctx, settingsToolRenameMigration); err != nil {
		t.Fatalf("migrate settings tool names: %v", err)
	}

	assertExactSystemOverrides(t, db, map[string]bool{"memory": false, "settings_agent_list": true})
	var retired int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM tool_override WHERE tool_name = ANY($1)`, settingsToolNamesRetiredByRename).Scan(&retired); err != nil {
		t.Fatalf("count retired Settings overrides: %v", err)
	}
	if retired != 0 {
		t.Errorf("retired Settings overrides after migration = %d, want 0 across every scope", retired)
	}
	for _, survivor := range []struct {
		tool, scope string
	}{
		{tool: "settings_provider_list", scope: "system_agent"},
		{tool: "memory", scope: "user"},
		{tool: "settings_mcp_server_list", scope: "user_agent"},
	} {
		var count int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM tool_override WHERE tool_name = $1 AND scope = $2`, survivor.tool, survivor.scope).Scan(&count); err != nil {
			t.Fatalf("count surviving %s override: %v", survivor.scope, err)
		}
		if count != 1 {
			t.Errorf("surviving %s override %q count = %d, want 1", survivor.scope, survivor.tool, count)
		}
	}
}

const (
	webFetchRenameBeforeMigration = 90000000000033
	webFetchRenameMigration       = 90000000000034
)

func TestWebFetchRenameDeletesOnlyRetiredName(t *testing.T) {
	db, provider := newTestDBAtMigration(t, webFetchRenameBeforeMigration)
	ctx := t.Context()
	seedToolOverride(t, db, "webfetch", false)
	seedToolOverride(t, db, "web_fetch", true)
	seedToolOverride(t, db, "web_search", false)

	if _, err := provider.UpTo(ctx, webFetchRenameMigration); err != nil {
		t.Fatalf("migrate web fetch tool name: %v", err)
	}

	assertExactSystemOverrides(t, db, map[string]bool{"web_fetch": true, "web_search": false})
}

const (
	webToolsRemovalBeforeMigration = 90000000000036
	webToolsRemovalMigration       = 90000000000037
)

func TestWebToolsRemovalDeletesOnlyRetiredNames(t *testing.T) {
	db, provider := newTestDBAtMigration(t, webToolsRemovalBeforeMigration)
	ctx := t.Context()
	seedToolOverride(t, db, "web_fetch", true)
	seedToolOverride(t, db, "web_search", false)
	seedToolOverride(t, db, "library_search", false)

	if _, err := provider.UpTo(ctx, webToolsRemovalMigration); err != nil {
		t.Fatalf("migrate web tools removal: %v", err)
	}

	assertExactSystemOverrides(t, db, map[string]bool{"library_search": false})
}

func seedToolOverride(t *testing.T, db *pgxpool.Pool, tool string, enabled bool) {
	t.Helper()
	seedScopedToolOverride(t, db, tool, "system", nil, nil, enabled)
}

func seedScopedToolOverride(t *testing.T, db *pgxpool.Pool, tool, scope string, userID, agentID any, enabled bool) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `
		INSERT INTO tool_override (tool_name, scope, user_id, agent_id, enabled)
		VALUES ($1, $2, $3, $4, $5)
	`, tool, scope, userID, agentID, enabled); err != nil {
		t.Fatalf("seed %s tool override %q: %v", scope, tool, err)
	}
}

// assertExactSystemOverrides compares the whole system-scoped table, not just
// the names the caller thought to name. A row that should have gone is as much
// a bug as one that should have stayed.
func assertExactSystemOverrides(t *testing.T, db *pgxpool.Pool, want map[string]bool) {
	t.Helper()
	rows, err := db.Query(context.Background(), `
		SELECT tool_name, enabled FROM tool_override WHERE scope = 'system'
	`)
	if err != nil {
		t.Fatalf("read system overrides: %v", err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var name string
		var enabled bool
		if err := rows.Scan(&name, &enabled); err != nil {
			t.Fatalf("scan system override: %v", err)
		}
		got[name] = enabled
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read system overrides: %v", err)
	}
	if !maps.Equal(got, want) {
		t.Errorf("system tool_override rows = %v, want %v", sortedPairs(got), sortedPairs(want))
	}
}

func sortedPairs(rows map[string]bool) []string {
	out := make([]string, 0, len(rows))
	for name, enabled := range rows {
		out = append(out, fmt.Sprintf("%s=%v", name, enabled))
	}
	sort.Strings(out)
	return out
}
