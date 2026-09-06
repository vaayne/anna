package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const pluginRetirementMigration42 = int64(90000000000042)

var retiredBuiltinTools = []struct {
	id        string
	namespace string
	backend   string
}{
	{id: "tool/mise", namespace: "mise", backend: "cli"},
	{id: "tool/xberg", namespace: "xberg", backend: "go"},
	{id: "tool/fd", namespace: "fd", backend: "cli"},
	{id: "tool/rg", namespace: "rg", backend: "cli"},
}

func TestRetireLegacyBuiltinToolsMigration(t *testing.T) {
	db, provider := newTestDBAtMigration(t, pluginCutoverMigration41)
	ctx := t.Context()
	seedRetiredBuiltinFixture(t, db)

	if _, err := provider.UpTo(ctx, pluginRetirementMigration42); err != nil {
		t.Fatalf("apply builtin retirement migration: %v", err)
	}

	for _, tool := range retiredBuiltinTools {
		for table, query := range map[string]string{
			"plugin":            `SELECT count(*) FROM plugin WHERE id = $1`,
			"plugin_override":   `SELECT count(*) FROM plugin_override WHERE plugin_id = $1`,
			"plugin_state":      `SELECT count(*) FROM plugin_state WHERE plugin_id = $1`,
			"plugin_definition": `SELECT count(*) FROM plugin_definition WHERE id = $1`,
			"plugin_config":     `SELECT count(*) FROM plugin_config WHERE plugin_id = $1`,
			"tool_override":     `SELECT count(*) FROM tool_override WHERE plugin_id = $1`,
		} {
			var count int
			if err := db.QueryRow(ctx, query, tool.id).Scan(&count); err != nil {
				t.Fatalf("count retired %s in %s: %v", tool.id, table, err)
			}
			if count != 0 {
				t.Errorf("retired %s rows in %s = %d, want 0", tool.id, table, count)
			}
		}
	}

	var kept int
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM plugin
		WHERE id = 'tool/keep'
	`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 1 {
		t.Fatalf("unrelated legacy plugin rows = %d, want 1", kept)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_definition WHERE id = 'tool/keep'`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 1 {
		t.Fatalf("unrelated common definition rows = %d, want 1", kept)
	}
	for table, query := range map[string]string{
		"plugin_override": `SELECT count(*) FROM plugin_override WHERE plugin_id = 'tool/keep'`,
		"plugin_state":    `SELECT count(*) FROM plugin_state WHERE plugin_id = 'tool/keep'`,
		"plugin_config":   `SELECT count(*) FROM plugin_config WHERE plugin_id = 'tool/keep'`,
		"tool_override":   `SELECT count(*) FROM tool_override WHERE plugin_id = 'tool/keep'`,
	} {
		if err := db.QueryRow(ctx, query).Scan(&kept); err != nil {
			t.Fatalf("count unrelated %s: %v", table, err)
		}
		if kept != 1 {
			t.Fatalf("unrelated %s rows = %d, want 1", table, kept)
		}
	}
	var latest int64
	if err := db.QueryRow(ctx, `SELECT version_id FROM goose_db_version ORDER BY id DESC LIMIT 1`).Scan(&latest); err != nil {
		t.Fatal(err)
	}
	if latest != pluginRetirementMigration42 {
		t.Fatalf("latest migration = %d, want 42", latest)
	}
}

func TestRetireLegacyBuiltinToolsMigrationFreshDatabase(t *testing.T) {
	db, provider := newTestDBAtMigration(t, pluginCutoverMigration40)
	ctx := t.Context()
	if _, err := provider.UpTo(ctx, pluginRetirementMigration42); err != nil {
		t.Fatalf("apply migrations 41-42 on fresh historical database: %v", err)
	}

	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin WHERE id = ANY($1::text[])`, retiredBuiltinIDs()).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("fresh database has retired legacy plugins = %d, want 0", count)
	}
	if err := db.QueryRow(ctx, `SELECT version_id FROM goose_db_version ORDER BY id DESC LIMIT 1`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if int64(count) != pluginRetirementMigration42 {
		t.Fatalf("fresh latest migration = %d, want 42", count)
	}
}

func TestRetireLegacyBuiltinToolsMigrationGuardsRollback(t *testing.T) {
	cases := []struct {
		name string
		seed func(*testing.T, *pgxpool.Pool, map[string]string)
	}{
		{
			name: "credential refs",
			seed: func(t *testing.T, db *pgxpool.Pool, configs map[string]string) {
				_, err := db.Exec(t.Context(), `UPDATE plugin_config SET credential_refs = '{"bearer":"vault-key"}'::jsonb WHERE id = $1::uuid`, configs["tool/mise"])
				if err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "manifest vault locator",
			seed: func(t *testing.T, db *pgxpool.Pool, _ map[string]string) {
				_, err := db.Exec(t.Context(), `UPDATE plugin_override SET session_env_vault_key = 'legacy-vault-key' WHERE plugin_id = 'tool/mise'`)
				if err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "MCP observation child",
			seed: func(t *testing.T, db *pgxpool.Pool, configs map[string]string) {
				_, err := db.Exec(t.Context(), `
					INSERT INTO mcp_connection_state (config_id, tools, status, config_revision)
					VALUES ($1::uuid, '[]'::jsonb, 'unknown', 1)
				`, configs["tool/mise"])
				if err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "foreign definition dependency",
			seed: func(t *testing.T, db *pgxpool.Pool, _ map[string]string) {
				_, err := db.Exec(t.Context(), `
					INSERT INTO plugin_definition (id, namespace, display_name, backend, source, implementation_key, spec, default_enabled)
					VALUES ('tool/dependent', 'dependent', 'Dependent', 'cli', 'custom', 'cli', '{"requires_plugin_ids":["tool/mise"]}'::jsonb, false)
				`)
				if err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "wrong legacy shape",
			seed: func(t *testing.T, db *pgxpool.Pool, _ map[string]string) {
				_, err := db.Exec(t.Context(), `UPDATE plugin SET kind = 'provider' WHERE id = 'tool/mise'`)
				if err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, provider := newTestDBAtMigration(t, pluginCutoverMigration41)
			ctx := t.Context()
			configs := seedRetiredBuiltinFixture(t, db)
			tc.seed(t, db, configs)

			if _, err := provider.UpTo(ctx, pluginRetirementMigration42); err == nil {
				t.Fatal("retirement migration unexpectedly succeeded")
			}
			var version int64
			var applied bool
			if err := db.QueryRow(ctx, `SELECT version_id, is_applied FROM goose_db_version ORDER BY id DESC LIMIT 1`).Scan(&version, &applied); err != nil {
				t.Fatal(err)
			}
			if version != pluginCutoverMigration41 || !applied {
				t.Fatalf("migration ledger = %d applied=%v, want 41/applied", version, applied)
			}
			var count int
			if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin WHERE id = 'tool/mise'`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("rollback lost legacy target plugin, rows = %d", count)
			}
		})
	}
}

func seedRetiredBuiltinFixture(t *testing.T, db *pgxpool.Pool) map[string]string {
	t.Helper()
	ctx := t.Context()
	userID := uuid.NewString()
	if _, err := db.Exec(ctx, `INSERT INTO auth_user (id, email) VALUES ($1, 'retirement-fixture@test.local')`, userID); err != nil {
		t.Fatal(err)
	}
	const agentID = "retirement-fixture-agent"
	if _, err := db.Exec(ctx, `INSERT INTO agent (id, name, workspace) VALUES ($1, 'Retirement Fixture Agent', '/tmp')`, agentID); err != nil {
		t.Fatal(err)
	}
	configs := make(map[string]string, len(retiredBuiltinTools))
	for i, tool := range retiredBuiltinTools {
		if _, err := db.Exec(ctx, `
			INSERT INTO plugin (id, kind, name, enabled, config)
			VALUES ($1, 'tool', $2, true, '{}'::jsonb)
		`, tool.id, tool.namespace); err != nil {
			t.Fatalf("seed legacy plugin %s: %v", tool.id, err)
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO plugin_override (plugin_id, enabled, config)
			VALUES ($1, false, '{"$sparse":true}'::text)
		`, tool.id); err != nil {
			t.Fatalf("seed legacy override %s: %v", tool.id, err)
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO plugin_state (plugin_id, scope_kind, scope_id, state_key, value)
			VALUES ($1, 'system', '', 'retired-state', '{}'::jsonb)
		`, tool.id); err != nil {
			t.Fatalf("seed legacy state %s: %v", tool.id, err)
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO plugin_definition (id, namespace, display_name, backend, source, implementation_key, spec, default_enabled)
			VALUES ($1, $2, $2, $3, 'builtin', $1, '{}'::jsonb, true)
		`, tool.id, tool.namespace, tool.backend); err != nil {
			t.Fatalf("seed common definition %s: %v", tool.id, err)
		}
		configID := uuid.NewString()
		configs[tool.id] = configID
		scope := "system"
		var configUserID, configAgentID any
		switch i {
		case 1:
			scope, configUserID = "user", userID
		case 2:
			scope, configAgentID = "system_agent", agentID
		case 3:
			scope, configUserID, configAgentID = "user_agent", userID, agentID
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO plugin_config (id, plugin_id, namespace, scope, user_id, agent_id, enabled, config, credential_refs)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, '{}'::jsonb, '{}'::jsonb)
		`, configID, tool.id, tool.namespace, scope, configUserID, configAgentID, i%2 == 0); err != nil {
			t.Fatalf("seed common config %s: %v", tool.id, err)
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO tool_override (tool_name, scope, user_id, agent_id, enabled, plugin_id, local_tool_name)
			VALUES (NULL, $2, $5, $6, $3, $1, $4)
		`, tool.id, scope, i%2 == 0, tool.namespace, configUserID, configAgentID); err != nil {
			t.Fatalf("seed scoped override %s: %v", tool.id, err)
		}
	}

	if _, err := db.Exec(ctx, `INSERT INTO plugin (id, kind, name, enabled, config) VALUES ('tool/keep', 'tool', 'keep', true, '{}'::jsonb)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO plugin_override (plugin_id, enabled, config) VALUES ('tool/keep', true, '{"keep":true}'::text)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO plugin_state (plugin_id, scope_kind, scope_id, state_key, value) VALUES ('tool/keep', 'system', '', 'keep', '{}'::jsonb)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO plugin_definition (id, namespace, display_name, backend, source, implementation_key, spec, default_enabled) VALUES ('tool/keep', 'keep', 'Keep', 'cli', 'custom', 'cli', '{}'::jsonb, false)`); err != nil {
		t.Fatal(err)
	}
	keepConfigID := uuid.NewString()
	if _, err := db.Exec(ctx, `INSERT INTO plugin_config (id, plugin_id, namespace, scope, enabled, config, credential_refs) VALUES ($1::uuid, 'tool/keep', 'keep', 'system', true, '{}'::jsonb, '{}'::jsonb)`, keepConfigID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO tool_override (tool_name, scope, enabled, plugin_id, local_tool_name) VALUES (NULL, 'system', true, 'tool/keep', 'keep')`); err != nil {
		t.Fatal(err)
	}
	return configs
}

func retiredBuiltinIDs() []string {
	ids := make([]string, 0, len(retiredBuiltinTools))
	for _, tool := range retiredBuiltinTools {
		ids = append(ids, tool.id)
	}
	return ids
}
