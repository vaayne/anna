package db

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/pkg/toolmeta"
)

func TestPreviewLegacyImportIsReadOnlyAndDoesNotWriteMarker(t *testing.T) {
	db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
	ctx := t.Context()
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin (id, kind, name, enabled, config)
		VALUES ('tool/test', 'tool', 'test', false, '{}'::jsonb)
	`); err != nil {
		t.Fatal(err)
	}
	catalog := plugin.NewCatalog()
	if err := catalog.Register(plugin.Definition{
		ID: "tool/test", Namespace: "test", DisplayName: "Test", Backend: plugin.BackendCLI,
		Source: plugin.SourceBuiltin, ImplementationKey: "tool/test", Spec: []byte(`{"name":"test"}`), DefaultEnabled: true, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	plan, err := plugin.PreviewLegacyImport(ctx, db, catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Configs) != 1 || plan.Configs[0].Enabled == nil || *plan.Configs[0].Enabled {
		t.Fatalf("normalized legacy config = %#v", plan.Configs)
	}
	var definitions, configs, markers int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_definition`).Scan(&definitions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_config`).Scan(&configs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM app_setting WHERE key = 'plugin_cutover_v1'`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if definitions != 0 || configs != 0 || markers != 0 {
		t.Fatalf("preview wrote target state: definitions=%d configs=%d markers=%d", definitions, configs, markers)
	}
}

func TestPreviewLegacyImportReportsUnexpectedMarkerAndMCPOverride(t *testing.T) {
	db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
	ctx := t.Context()
	catalog := plugin.NewCatalog()
	if _, err := db.Exec(ctx, `INSERT INTO app_setting (key, value) VALUES ('plugin_cutover_v1', 'future')`); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.PreviewLegacyImport(ctx, db, catalog, nil); !errors.Is(err, plugin.ErrLegacyMigrationConflict) {
		t.Fatalf("unexpected marker error = %v", err)
	}

	if _, err := db.Exec(ctx, `DELETE FROM app_setting WHERE key = 'plugin_cutover_v1'`); err != nil {
		t.Fatal(err)
	}
	const registrationID = "0198f9a4-1b2c-7def-8123-456789abcdef"
	if _, err := db.Exec(ctx, `
		INSERT INTO mcp_server (id, scope, name, url, transport, auth_type, enabled, metadata, tools, credential_mode)
		VALUES ($1, 'system', 'github', 'https://mcp.example.test', 'sse', 'none', true, '{}'::jsonb, '[{"name":"create_issue"}]'::jsonb, 'shared')
	`, registrationID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO tool_override (tool_name, scope, enabled)
		VALUES ('mcp__github__create_issue', 'system', false)
	`); err != nil {
		t.Fatal(err)
	}
	plan, err := plugin.PreviewLegacyImport(ctx, db, catalog, nil)
	if err != nil {
		t.Fatalf("MCP override preview error = %v", err)
	}
	if len(plan.ToolOverrides) != 1 || plan.ToolOverrides[0].PluginID != "custom/"+registrationID || plan.ToolOverrides[0].LocalTool != "create_issue" || plan.ToolOverrides[0].Enabled {
		t.Fatalf("MCP override mapping = %#v", plan.ToolOverrides)
	}
	var definitions, configs int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_definition`).Scan(&definitions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_config`).Scan(&configs); err != nil {
		t.Fatal(err)
	}
	if definitions != 0 || configs != 0 {
		t.Fatalf("failed preview wrote target state: definitions=%d configs=%d", definitions, configs)
	}
}

func TestPreviewLegacyImportDerivesOAuthSecretLocatorFromVaultPresence(t *testing.T) {
	const registrationID = "0198f9a4-1b2c-7def-8123-456789abcdef"
	const secretName = "MCP_OAUTH_CLIENT_0198F9A4_1B2C_7DEF_8123_456789ABCDEF"
	for _, test := range []struct {
		name          string
		metadata      string
		insertSecret  bool
		wantSecretRef bool
	}{
		{name: "secret present", metadata: `{"oauth":{"client_id":"public-client"}}`, insertSecret: true, wantSecretRef: true},
		{name: "public client only", metadata: `{"oauth":{"client_id":"public-client"}}`, wantSecretRef: false},
		{name: "stale metadata locator without secret", metadata: `{"oauth":{"client_id":"public-client","client_secret_ref":"MCP_OAUTH_CLIENT_0198F9A4_1B2C_7DEF_8123_456789ABCDEF"}}`, wantSecretRef: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
			ctx := t.Context()
			if _, err := db.Exec(ctx, `
				INSERT INTO mcp_server (id, scope, name, url, transport, auth_type,
					enabled, metadata, tools, credential_mode)
				VALUES ($1, 'system', 'oauth-server', 'https://mcp.example.test', 'sse', 'oauth',
					true, $2::jsonb, '[]'::jsonb, 'shared')
			`, registrationID, test.metadata); err != nil {
				t.Fatal(err)
			}
			if test.insertSecret {
				if _, err := db.Exec(ctx, `
					INSERT INTO vault_entry (scope, name, ciphertext)
					VALUES ('system', $1, 'opaque-ciphertext')
				`, secretName); err != nil {
					t.Fatal(err)
				}
			}
			plan, err := plugin.PreviewLegacyImport(ctx, db, plugin.NewCatalog(), nil)
			if err != nil {
				t.Fatal(err)
			}
			refs := string(plan.Configs[0].CredentialRefs)
			if got := strings.Contains(refs, "oauth_client_secret"); got != test.wantSecretRef {
				t.Fatalf("OAuth secret locator present=%t, refs=%s", got, refs)
			}
			if test.wantSecretRef && !strings.Contains(refs, secretName) {
				t.Fatalf("derived OAuth secret locator missing, refs=%s", refs)
			}
		})
	}
}

func TestImportLegacyStateRejectsOAuthSecretWithoutClientID(t *testing.T) {
	db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
	ctx := t.Context()
	const registrationID = "0198f9a4-1b2c-7def-8123-456789abcdef"
	const secretName = "MCP_OAUTH_CLIENT_0198F9A4_1B2C_7DEF_8123_456789ABCDEF"
	if _, err := db.Exec(ctx, `
		INSERT INTO mcp_server (id, scope, name, url, transport, auth_type,
			enabled, metadata, tools, credential_mode)
		VALUES ($1, 'system', 'oauth-server', 'https://mcp.example.test', 'sse', 'oauth',
			true, '{"oauth":{}}'::jsonb, '[]'::jsonb, 'shared')
	`, registrationID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO vault_entry (scope, name, ciphertext)
		VALUES ('system', $1, 'opaque-ciphertext')
	`, secretName); err != nil {
		t.Fatal(err)
	}
	err := plugin.ImportLegacyState(ctx, db, plugin.NewCatalog(), nil)
	if !errors.Is(err, plugin.ErrLegacyMigrationConflict) || !strings.Contains(err.Error(), "client_id") {
		t.Fatalf("OAuth secret without client_id import error = %v", err)
	}
	var definitions, configs, markers int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_definition`).Scan(&definitions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_config`).Scan(&configs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM app_setting WHERE key='plugin_cutover_v1'`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if definitions != 0 || configs != 0 || markers != 0 {
		t.Fatalf("invalid OAuth import wrote state: definitions=%d configs=%d markers=%d", definitions, configs, markers)
	}
}

func TestImportLegacyStateRejectsUnexpectedCredentialRefsBeforeMarker(t *testing.T) {
	for _, test := range []legacyMCPCredentialRefTest{
		{name: "oauth metadata", authType: "oauth", metadata: `{"oauth":{"client_id":"public-client","client_secret_ref":"legacy-secret"}}`},
		{name: "bearer locator", authType: "bearer", credential: "MCP_TOKEN_legacy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
			ctx := t.Context()
			if _, err := db.Exec(ctx, `
				INSERT INTO mcp_server (id, scope, name, url, transport, auth_type,
					credential_ref, enabled, metadata, tools, credential_mode)
				VALUES ('0198f9a4-1b2c-7def-8123-456789abcdef', 'system', 'oauth-server',
					'https://mcp.example.test', 'sse', $1, $2, true, $3::jsonb, '[]'::jsonb, 'shared')
			`, test.authType, test.credential, test.metadataOrEmpty()); err != nil {
				t.Fatal(err)
			}
			err := plugin.ImportLegacyState(ctx, db, plugin.NewCatalog(), nil)
			if !errors.Is(err, plugin.ErrLegacyMigrationConflict) {
				t.Fatalf("unexpected credential ref error = %v", err)
			}
			var definitions, configs, markers int
			if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_definition`).Scan(&definitions); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_config`).Scan(&configs); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(ctx, `SELECT count(*) FROM app_setting WHERE key='plugin_cutover_v1'`).Scan(&markers); err != nil {
				t.Fatal(err)
			}
			if definitions != 0 || configs != 0 || markers != 0 {
				t.Fatalf("rejected import wrote state: definitions=%d configs=%d markers=%d", definitions, configs, markers)
			}
		})
	}
}

type legacyMCPCredentialRefTest struct {
	name       string
	authType   string
	credential string
	metadata   string
}

func (test legacyMCPCredentialRefTest) metadataOrEmpty() string {
	if test.metadata == "" {
		return `{}`
	}
	return test.metadata
}

func TestPreviewLegacyImportMigratesChannelCapabilityWithoutCredentials(t *testing.T) {
	db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
	ctx := t.Context()
	const channelConfig = `{"app_id":"instance-app","app_secret":"instance-secret"}`
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin (id, kind, name, enabled, config)
		VALUES ('channel/feishu', 'channel', 'feishu', true, '{"app_id":"mirror-app","app_secret":"fakeappsecret"}'::jsonb)
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO channel (id, name, type, enabled, config)
		VALUES ('feishu-work', 'Work Feishu', 'feishu', true, $1)
	`, channelConfig); err != nil {
		t.Fatal(err)
	}
	catalog := plugin.NewCatalog()
	if err := catalog.Register(plugin.Definition{
		ID: "channel/feishu", Namespace: "feishu", DisplayName: "Feishu", Backend: plugin.BackendGo,
		Source: plugin.SourceBuiltin, ImplementationKey: "channel/feishu", Spec: []byte(`{}`), DefaultEnabled: true, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}

	plan, err := plugin.PreviewLegacyImport(ctx, db, catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Configs) != 1 || plan.Configs[0].PluginID != "channel/feishu" || plan.Configs[0].Enabled == nil || !*plan.Configs[0].Enabled || string(plan.Configs[0].Payload) != `{}` {
		t.Fatalf("channel capability = %#v", plan.Configs)
	}
	planBytes, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(planBytes), "fakeappsecret") || strings.Contains(string(planBytes), "instance-secret") {
		t.Fatal("channel credentials escaped into preview plan")
	}
	var id, name, channelType, gotConfig string
	var enabled bool
	if err := db.QueryRow(ctx, `SELECT id, name, type, enabled, config FROM channel WHERE id = 'feishu-work'`).Scan(&id, &name, &channelType, &enabled, &gotConfig); err != nil {
		t.Fatal(err)
	}
	if id != "feishu-work" || name != "Work Feishu" || channelType != "feishu" || !enabled || gotConfig != channelConfig {
		t.Fatal("source channel row changed")
	}
}

func TestPreviewLegacyImportRejectsChannelPayloadWithoutInstance(t *testing.T) {
	db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
	ctx := t.Context()
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin (id, kind, name, enabled, config)
		VALUES ('channel/feishu', 'channel', 'feishu', true, '{"app_secret":"fakeappsecret"}'::jsonb)
	`); err != nil {
		t.Fatal(err)
	}
	catalog := plugin.NewCatalog()
	if err := catalog.Register(plugin.Definition{
		ID: "channel/feishu", Namespace: "feishu", DisplayName: "Feishu", Backend: plugin.BackendGo,
		Source: plugin.SourceBuiltin, ImplementationKey: "channel/feishu", Spec: []byte(`{}`), DefaultEnabled: true, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := plugin.PreviewLegacyImport(ctx, db, catalog, nil)
	if !errors.Is(err, plugin.ErrLegacyMigrationConflict) || !strings.Contains(err.Error(), "channel/feishu") {
		t.Fatalf("missing channel instance error = %v", err)
	}
	if strings.Contains(err.Error(), "fakeappsecret") {
		t.Fatal("channel secret escaped into migration error")
	}
}

func TestImportLegacyStateWritesDefinitionsConfigsAndSharedObservation(t *testing.T) {
	db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
	ctx := t.Context()
	preparePluginPolicyCutoverSchema(t, db)
	catalog := plugin.NewCatalog()
	builtin := pluginDefinition("builtin/import", "import", true)
	if err := catalog.Register(builtin); err != nil {
		t.Fatal(err)
	}
	const mcpID = "0198f9a4-1b2c-7def-8123-456789abcdef"
	const mirror = `{"provider":"legacy"}`
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin (id, kind, name, enabled, config)
		VALUES ('builtin/import', 'tool', 'import', false, $1::jsonb)
	`, mirror); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO mcp_server (id, scope, name, url, transport, auth_type, enabled,
			metadata, tools, status, status_error, credential_mode)
		VALUES ($1, 'system', 'github', 'https://mcp.example.test', 'sse', 'none', true,
			'{}'::jsonb, '[{"name":"issues"}]'::jsonb, 'ok', '', 'shared')
	`, mcpID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO channel (id, name, type, enabled, config)
		VALUES ('feishu-work', 'Work', 'feishu', true, '{"app_secret":"source-only"}')
	`); err != nil {
		t.Fatal(err)
	}
	if err := plugin.ImportLegacyState(ctx, db, catalog, nil); errors.Is(err, plugin.ErrOAuthForeignKeySchema) {
		t.Skip("OAuth FK final cutover migration is not present in this preparation schema")
	} else if err != nil {
		t.Fatal(err)
	}
	var marker string
	if err := db.QueryRow(ctx, `SELECT value FROM app_setting WHERE key = 'plugin_cutover_v1'`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != "v1" {
		t.Fatalf("marker = %q", marker)
	}
	var definitionCount, configCount int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_definition`).Scan(&definitionCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_config`).Scan(&configCount); err != nil {
		t.Fatal(err)
	}
	if definitionCount != 2 || configCount != 2 {
		t.Fatalf("target counts = definitions %d configs %d", definitionCount, configCount)
	}
	var enabled bool
	if err := db.QueryRow(ctx, `SELECT enabled FROM plugin_config WHERE plugin_id = 'builtin/import' AND scope = 'system'`).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("legacy disabled capability was not imported")
	}
	var status, statusError, tools string
	if err := db.QueryRow(ctx, `
		SELECT status, status_error, tools::text FROM mcp_connection_state
		WHERE config_id = $1::uuid AND credential_user_id IS NULL
	`, mcpID).Scan(&status, &statusError, &tools); err != nil {
		t.Fatal(err)
	}
	if status != "ok" || statusError != "" || tools != `[{"name": "issues"}]` {
		t.Fatalf("shared observation = status=%q error=%q tools=%q", status, statusError, tools)
	}
	var gotMirror string
	if err := db.QueryRow(ctx, `SELECT config::text FROM plugin WHERE id = 'builtin/import'`).Scan(&gotMirror); err != nil {
		t.Fatal(err)
	}
	var gotMirrorObject map[string]any
	if err := json.Unmarshal([]byte(gotMirror), &gotMirrorObject); err != nil || gotMirrorObject["provider"] != "legacy" || len(gotMirrorObject) != 1 {
		t.Fatalf("legacy plugin row changed: %q", gotMirror)
	}
	if err := plugin.ImportLegacyState(ctx, db, catalog, nil); !errors.Is(err, plugin.ErrImportComplete) {
		t.Fatalf("second import = %v", err)
	}
}

func TestImportLegacyStateDoesNotImportOwnerlessPerUserObservation(t *testing.T) {
	db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
	ctx := t.Context()
	preparePluginPolicyCutoverSchema(t, db)
	user := insertPluginUser(t, db, "legacy-import-user@example.test", false)
	catalog := plugin.NewCatalog()
	if err := catalog.Register(pluginDefinition("builtin/import", "import", true)); err != nil {
		t.Fatal(err)
	}
	const mcpID = "0198f9a4-1b2c-7def-8123-456789abcdea"
	if _, err := db.Exec(ctx, `
		INSERT INTO mcp_server (id, scope, user_id, name, url, transport, auth_type,
			enabled, metadata, tools, status, credential_mode)
		VALUES ($1, 'user', $2, 'private', 'https://mcp.example.test', 'sse', 'none',
			true, '{}'::jsonb, '[{"name":"private"}]'::jsonb, 'ok', 'per_user')
	`, mcpID, user.UserID()); err != nil {
		t.Fatal(err)
	}
	if err := plugin.ImportLegacyState(ctx, db, catalog, nil); errors.Is(err, plugin.ErrOAuthForeignKeySchema) {
		t.Skip("OAuth FK final cutover migration is not present in this preparation schema")
	} else if err != nil {
		t.Fatal(err)
	}
	var observations int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM mcp_connection_state`).Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if observations != 0 {
		t.Fatalf("per-user observation imported without probe owner: %d", observations)
	}
	var configCount int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_config WHERE id = $1::uuid`, mcpID).Scan(&configCount); err != nil {
		t.Fatal(err)
	}
	if configCount != 1 {
		t.Fatalf("per-user registration config count = %d", configCount)
	}
}

func TestImportLegacyStateRollsBackBeforeMarkerOnPolicyDependency(t *testing.T) {
	db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
	ctx := t.Context()
	catalog := plugin.NewCatalog()
	if err := catalog.Register(pluginDefinition("builtin/import", "import", true)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin (id, kind, name, enabled, config)
		VALUES ('builtin/import', 'tool', 'import', true, '{}'::jsonb)
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO mcp_server (id, scope, name, url, transport, auth_type, enabled, metadata, tools, credential_mode)
		VALUES ('0198f9a4-1b2c-7def-8123-456789abcdec', 'system', 'missing', 'https://mcp.example.test', 'sse', 'none', true, '{}'::jsonb, '[{"name":"tool"}]'::jsonb, 'shared')
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO tool_override (tool_name, scope, enabled)
		VALUES ('mcp__missing__tool', 'system', false)
	`); err != nil {
		t.Fatal(err)
	}
	if err := plugin.ImportLegacyState(ctx, db, catalog, nil); err != nil {
		t.Fatalf("import legacy policy = %v", err)
	}
	var definitions, configs, markers int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_definition`).Scan(&definitions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_config`).Scan(&configs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM app_setting WHERE key = 'plugin_cutover_v1'`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if definitions == 0 || configs == 0 || markers != 1 {
		t.Fatalf("imported policy target state = definitions:%d configs:%d markers:%d", definitions, configs, markers)
	}
}

func TestImportLegacyStateWritesPluginToolOverrideWithExactIdentity(t *testing.T) {
	db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
	ctx := t.Context()
	preparePluginPolicyCutoverSchema(t, db)
	catalog := plugin.NewCatalog()
	user := insertPluginUser(t, db, "legacy-import-policy@example.test", false)
	const registrationID = "0198f9a4-1b2c-7def-8123-456789abcdea"
	if _, err := db.Exec(ctx, `
		INSERT INTO mcp_server (id, scope, name, url, transport, auth_type, enabled,
			metadata, tools, status, credential_mode)
		VALUES ($1, 'system', 'github', 'https://mcp.example.test', 'sse', 'none',
			true, '{}'::jsonb, '[{"name":"create_issue"}]'::jsonb, 'ok', 'shared')
	`, registrationID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO tool_override (tool_name, scope, enabled)
		VALUES ('mcp__github__create_issue', 'system', false)
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO tool_override (tool_name, scope, user_id, enabled)
		VALUES ('mcp__github__create_issue', 'user', $1, true)
	`, user.UserID()); err != nil {
		t.Fatal(err)
	}
	if err := plugin.ImportLegacyState(ctx, db, catalog, nil); err != nil {
		t.Fatal(err)
	}
	var toolName pgtype.Text
	var pluginID, localName, scope string
	var enabled bool
	if err := db.QueryRow(ctx, `
		SELECT tool_name, plugin_id, local_tool_name, scope, enabled
		FROM tool_override WHERE plugin_id = $1 AND scope = 'system'
	`, "custom/"+registrationID).Scan(&toolName, &pluginID, &localName, &scope, &enabled); err != nil {
		t.Fatal(err)
	}
	if toolName.Valid || pluginID != "custom/"+registrationID || localName != "create_issue" || scope != "system" || enabled {
		t.Fatalf("imported plugin policy = tool=%#v plugin=%q local=%q scope=%q enabled=%v", toolName, pluginID, localName, scope, enabled)
	}
	var gotUser string
	if err := db.QueryRow(ctx, `
		SELECT user_id::text FROM tool_override
		WHERE plugin_id = $1 AND scope = 'user'
	`, "custom/"+registrationID).Scan(&gotUser); err != nil {
		t.Fatal(err)
	}
	if gotUser != string(user.UserID()) {
		t.Fatalf("imported user policy owner = %q, want %q", gotUser, user.UserID())
	}
}

func TestImportLegacyStateRollsBackAmbiguousPluginToolOverride(t *testing.T) {
	db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
	ctx := t.Context()
	preparePluginPolicyCutoverSchema(t, db)
	catalog := plugin.NewCatalog()
	user := insertPluginUser(t, db, "legacy-import-ambiguous@example.test", false)
	const firstID = "0198f9a4-1b2c-7def-8123-456789abcdeb"
	const secondID = "0198f9a4-1b2c-7def-8123-456789abcdec"
	for _, registration := range []struct {
		id, scope, userID string
	}{
		{id: firstID, scope: "system"},
		{id: secondID, scope: "user", userID: string(user.UserID())},
	} {
		if _, err := db.Exec(ctx, `
			INSERT INTO mcp_server (id, scope, name, url, transport, auth_type, enabled,
				user_id, metadata, tools, status, credential_mode)
			VALUES ($1, $2, 'github', 'https://mcp.example.test', 'sse', 'none',
				true, NULLIF($3::text, '')::uuid,
				'{}'::jsonb, '[{"name":"create_issue"}]'::jsonb, 'ok', 'shared')
		`, registration.id, registration.scope, registration.userID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO tool_override (tool_name, scope, enabled)
		VALUES ('mcp__github__create_issue', 'system', false)
	`); err != nil {
		t.Fatal(err)
	}

	err := plugin.ImportLegacyState(ctx, db, catalog, nil)
	if !errors.Is(err, plugin.ErrLegacyMigrationConflict) {
		t.Fatalf("ambiguous plugin policy error = %v", err)
	}
	var definitions, configs, markers int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_definition`).Scan(&definitions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_config`).Scan(&configs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM app_setting WHERE key = 'plugin_cutover_v1'`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if definitions != 0 || configs != 0 || markers != 0 {
		t.Fatalf("ambiguous import wrote target state: definitions=%d configs=%d markers=%d", definitions, configs, markers)
	}
}

func TestImportLegacyStateVerifiesKnownCoreAndKeepsRow(t *testing.T) {
	db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
	ctx := t.Context()
	preparePluginPolicyCutoverSchema(t, db)
	if _, err := db.Exec(ctx, `
		INSERT INTO tool_override (tool_name, scope, enabled)
		VALUES ('goal_create', 'system', false)
	`); err != nil {
		t.Fatal(err)
	}
	metadata := toolmeta.NewRegistry(toolmeta.ActionTool{Name: "goal_create"})
	if err := plugin.ImportLegacyState(ctx, db, plugin.NewCatalog(), metadata); err != nil {
		t.Fatalf("known core policy import = %v", err)
	}
	var toolName, scope string
	var enabled bool
	if err := db.QueryRow(ctx, `
		SELECT tool_name, scope, enabled FROM tool_override
		WHERE tool_name = 'goal_create' AND plugin_id IS NULL
	`).Scan(&toolName, &scope, &enabled); err != nil {
		t.Fatal(err)
	}
	if toolName != "goal_create" || scope != "system" || enabled {
		t.Fatalf("core policy changed during verification: name=%q scope=%q enabled=%v", toolName, scope, enabled)
	}
}

func TestImportLegacyStateRejectsUnknownCoreBeforeWrites(t *testing.T) {
	db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
	ctx := t.Context()
	preparePluginPolicyCutoverSchema(t, db)
	if _, err := db.Exec(ctx, `
		INSERT INTO tool_override (tool_name, scope, enabled)
		VALUES ('vllm', 'system', false)
	`); err != nil {
		t.Fatal(err)
	}
	metadata := toolmeta.NewRegistry(toolmeta.ActionTool{Name: "goal_create"})
	err := plugin.ImportLegacyState(ctx, db, plugin.NewCatalog(), metadata)
	if !errors.Is(err, plugin.ErrLegacyMigrationConflict) {
		t.Fatalf("reserved-but-not-executable core name should be rejected, got %v", err)
	}
	var definitions, configs, markers int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_definition`).Scan(&definitions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_config`).Scan(&configs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM app_setting WHERE key = 'plugin_cutover_v1'`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if definitions != 0 || configs != 0 || markers != 0 {
		t.Fatalf("unknown core policy wrote target state: definitions=%d configs=%d markers=%d", definitions, configs, markers)
	}
}

func TestImportLegacyStateRejectsCoreWithoutTrustedMetadata(t *testing.T) {
	db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
	ctx := t.Context()
	preparePluginPolicyCutoverSchema(t, db)
	if _, err := db.Exec(ctx, `
		INSERT INTO tool_override (tool_name, scope, enabled)
		VALUES ('goal_create', 'system', false)
	`); err != nil {
		t.Fatal(err)
	}
	err := plugin.ImportLegacyState(ctx, db, plugin.NewCatalog(), nil)
	if !errors.Is(err, plugin.ErrToolOverrideSchema) {
		t.Fatalf("nil core metadata should be rejected, got %v", err)
	}
}

func TestImportLegacyStateRejectsCoreRowWithPluginIdentity(t *testing.T) {
	db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
	ctx := t.Context()
	preparePluginPolicyCutoverSchema(t, db)
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin_definition (
			id, namespace, display_name, backend, source, implementation_key,
			spec, default_enabled, revision
		) VALUES ('system/dual-row', 'dual-row', 'Dual row fixture', 'go', 'builtin', 'dual-row', '{}', false, 1)
	`); err != nil {
		t.Fatal(err)
	}
	var overrideID string
	if err := db.QueryRow(ctx, `
		INSERT INTO tool_override (tool_name, scope, enabled)
		VALUES ('goal_create', 'system', false)
		RETURNING id::text
	`).Scan(&overrideID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		ALTER TABLE tool_override DROP CONSTRAINT tool_override_identity_check;
		ALTER TABLE tool_override ADD CONSTRAINT tool_override_identity_check CHECK (true)
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		UPDATE tool_override
		SET plugin_id = 'system/dual-row', local_tool_name = 'goal_create'
		WHERE id = $1::uuid
	`, overrideID); err != nil {
		t.Fatal(err)
	}
	metadata := toolmeta.NewRegistry(toolmeta.ActionTool{Name: "goal_create"})
	err := plugin.ImportLegacyState(ctx, db, plugin.NewCatalog(), metadata)
	if !errors.Is(err, plugin.ErrLegacyMigrationConflict) {
		t.Fatalf("dual core/plugin identity should be rejected, got %v", err)
	}
	var configs, markers int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_config`).Scan(&configs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM app_setting WHERE key = 'plugin_cutover_v1'`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if configs != 0 || markers != 0 {
		t.Fatalf("dual identity rejection wrote target state: configs=%d markers=%d", configs, markers)
	}
}

func preparePluginPolicyCutoverSchema(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	ctx := t.Context()
	var applied bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM goose_db_version WHERE version_id = 90000000000041 AND is_applied)`).Scan(&applied); err != nil {
		t.Fatalf("read migration 41 ledger: %v", err)
	}
	if !applied {
		t.Fatal("migration 41 is not applied in the test database")
	}
	for _, column := range []string{"plugin_id", "local_tool_name"} {
		if !columnExists(t, db, "tool_override", column) {
			t.Fatalf("migration 41 column tool_override.%s is missing", column)
		}
	}
	var indexCount int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname IN ('uniq_tool_override_core_identity', 'uniq_tool_override_plugin_identity')`).Scan(&indexCount); err != nil {
		t.Fatalf("read migration 41 policy indexes: %v", err)
	}
	if indexCount != 2 {
		t.Fatalf("migration 41 policy indexes = %d, want 2", indexCount)
	}
}

func TestImportLegacyStateRequiresAppliedMigration41Ledger(t *testing.T) {
	db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
	ctx := t.Context()
	preparePluginPolicyCutoverSchema(t, db)
	if _, err := db.Exec(ctx, `DELETE FROM goose_db_version WHERE version_id = 90000000000041`); err != nil {
		t.Fatal(err)
	}
	if err := plugin.ImportLegacyState(ctx, db, plugin.NewCatalog(), nil); !errors.Is(err, plugin.ErrToolOverrideSchema) {
		t.Fatalf("missing migration ledger error = %v", err)
	}
}

func TestImportLegacyStateRejectsUnappliedLatestMigration41Ledger(t *testing.T) {
	db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
	ctx := t.Context()
	preparePluginPolicyCutoverSchema(t, db)
	if _, err := db.Exec(ctx, `
		INSERT INTO goose_db_version (version_id, is_applied)
		VALUES (90000000000041, false)
	`); err != nil {
		t.Fatal(err)
	}
	if err := plugin.ImportLegacyState(ctx, db, plugin.NewCatalog(), nil); !errors.Is(err, plugin.ErrToolOverrideSchema) {
		t.Fatalf("unapplied latest migration ledger error = %v", err)
	}
}

func TestImportLegacyStateAcceptsAppliedMigration41WithLaterMigrations(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	if err := plugin.ImportLegacyState(ctx, db, plugin.NewCatalog(), nil); err != nil {
		t.Fatalf("fresh database import: %v", err)
	}

	var marker string
	if err := db.QueryRow(ctx, `SELECT value FROM app_setting WHERE key = 'plugin_cutover_v1'`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != "v1" {
		t.Fatalf("cutover marker = %q, want v1", marker)
	}
}

func TestImportLegacyStateRefusesPreparationSchema(t *testing.T) {
	db, _ := newTestDBAtMigration(t, pluginCutoverMigration40)
	ctx := t.Context()
	catalog := plugin.NewCatalog()
	if err := catalog.Register(pluginDefinition("builtin/import", "import", true)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin (id, kind, name, enabled, config)
		VALUES ('builtin/import', 'tool', 'import', true, '{}'::jsonb)
	`); err != nil {
		t.Fatal(err)
	}
	err := plugin.ImportLegacyState(ctx, db, catalog, nil)
	if !errors.Is(err, plugin.ErrToolOverrideSchema) {
		t.Fatalf("preparation schema error = %v", err)
	}
	var definitions, configs, markers int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_definition`).Scan(&definitions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_config`).Scan(&configs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM app_setting WHERE key = 'plugin_cutover_v1'`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if definitions != 0 || configs != 0 || markers != 0 {
		t.Fatalf("preparation schema import wrote target state: definitions=%d configs=%d markers=%d", definitions, configs, markers)
	}
}

func TestImportLegacyStateRequiresNamedPolicyIndexesWithNoLegacyPolicies(t *testing.T) {
	db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
	ctx := t.Context()
	preparePluginPolicyCutoverSchema(t, db)
	if _, err := db.Exec(ctx, `DROP INDEX uniq_tool_override_plugin_identity`); err != nil {
		t.Fatal(err)
	}
	if err := plugin.ImportLegacyState(ctx, db, plugin.NewCatalog(), nil); !errors.Is(err, plugin.ErrToolOverrideSchema) {
		t.Fatalf("missing plugin policy index error = %v", err)
	}
	var markers int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM app_setting WHERE key = 'plugin_cutover_v1'`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if markers != 0 {
		t.Fatalf("schema readiness failure wrote marker: %d", markers)
	}
}

func TestImportLegacyStateRejectsNonUniqueNamedPolicyIndexWithNoLegacyPolicies(t *testing.T) {
	db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
	ctx := t.Context()
	preparePluginPolicyCutoverSchema(t, db)
	if _, err := db.Exec(ctx, `DROP INDEX uniq_tool_override_plugin_identity`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		CREATE INDEX uniq_tool_override_plugin_identity
			ON tool_override (plugin_id, local_tool_name, scope, user_id, agent_id)
			WHERE tool_name IS NULL
	`); err != nil {
		t.Fatal(err)
	}
	if err := plugin.ImportLegacyState(ctx, db, plugin.NewCatalog(), nil); !errors.Is(err, plugin.ErrToolOverrideSchema) {
		t.Fatalf("non-unique plugin policy index error = %v", err)
	}
	var markers int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM app_setting WHERE key = 'plugin_cutover_v1'`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if markers != 0 {
		t.Fatalf("schema readiness failure wrote marker: %d", markers)
	}
}

func TestImportLegacyStateRequiresExactOAuthForeignKey(t *testing.T) {
	db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
	ctx := t.Context()
	preparePluginPolicyCutoverSchema(t, db)
	if _, err := db.Exec(ctx, `
		ALTER TABLE mcp_oauth_flow DROP CONSTRAINT mcp_oauth_flow_server_id_plugin_config_fkey;
		ALTER TABLE mcp_oauth_flow
			ADD CONSTRAINT mcp_oauth_flow_server_id_fkey
			FOREIGN KEY (server_id) REFERENCES mcp_server(id) ON DELETE CASCADE;
	`); err != nil {
		t.Fatal(err)
	}
	if err := plugin.ImportLegacyState(ctx, db, plugin.NewCatalog(), nil); !errors.Is(err, plugin.ErrOAuthForeignKeySchema) {
		t.Fatalf("wrong OAuth foreign key error = %v", err)
	}
}

func TestImportLegacyStateRejectsAdditionalOAuthForeignKey(t *testing.T) {
	db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
	ctx := t.Context()
	preparePluginPolicyCutoverSchema(t, db)
	if _, err := db.Exec(ctx, `
		ALTER TABLE public.mcp_oauth_flow
			ADD CONSTRAINT mcp_oauth_flow_server_id_legacy_extra_fkey
			FOREIGN KEY (server_id) REFERENCES public.mcp_server(id) ON DELETE CASCADE
	`); err != nil {
		t.Fatal(err)
	}
	if err := plugin.ImportLegacyState(ctx, db, plugin.NewCatalog(), nil); !errors.Is(err, plugin.ErrOAuthForeignKeySchema) {
		t.Fatalf("additional OAuth foreign key error = %v", err)
	}
	var markers int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM app_setting WHERE key = 'plugin_cutover_v1'`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if markers != 0 {
		t.Fatalf("additional OAuth foreign key wrote marker: %d", markers)
	}
}

func TestImportLegacyStateValidatesRetargetedOAuthFlowAfterUUIDConfigImport(t *testing.T) {
	db, provider := newTestDBAtMigration(t, pluginCutoverMigration40)
	ctx := t.Context()
	user := insertPluginUser(t, db, "legacy-import-oauth@example.test", false)
	catalog := plugin.NewCatalog()
	if err := catalog.Register(pluginDefinition("builtin/import", "import", true)); err != nil {
		t.Fatal(err)
	}
	const mcpID = "0198f9a4-1b2c-7def-8123-456789abcdeb"
	if _, err := db.Exec(ctx, `
		INSERT INTO mcp_server (id, scope, name, url, transport, auth_type,
			enabled, metadata, tools, status, credential_mode)
		VALUES ($1, 'system', 'oauth-server', 'https://mcp.example.test', 'sse', 'oauth',
			true, '{}'::jsonb, '[]'::jsonb, 'needs_auth', 'shared')
	`, mcpID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO mcp_oauth_flow (
			server_id, user_id, credential_scope, pkce_verifier, oauth_config, expires_at
		) VALUES ($1, $2, 'system', 'verifier', '{"client_id":"client"}'::jsonb, now() + interval '5 minutes')
	`, mcpID, user.UserID()); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, pluginCutoverMigration41); err != nil {
		t.Fatalf("apply embedded migration 41: %v", err)
	}
	preparePluginPolicyCutoverSchema(t, db)
	if err := plugin.ImportLegacyState(ctx, db, catalog, nil); err != nil {
		t.Fatal(err)
	}
	var marker string
	if err := db.QueryRow(ctx, `SELECT value FROM app_setting WHERE key = 'plugin_cutover_v1'`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != "v1" {
		t.Fatalf("marker = %q", marker)
	}
	var validated bool
	if err := db.QueryRow(ctx, `
		SELECT convalidated FROM pg_constraint WHERE conname = 'mcp_oauth_flow_server_id_plugin_config_fkey'
	`).Scan(&validated); err != nil {
		t.Fatal(err)
	}
	if !validated {
		t.Fatal("retargeted OAuth FK remained NOT VALID")
	}
	var configCount int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_config WHERE id = $1::uuid`, mcpID).Scan(&configCount); err != nil {
		t.Fatal(err)
	}
	if configCount != 1 {
		t.Fatalf("UUID-preserving MCP config count = %d", configCount)
	}
}

func TestImportLegacyStateRejectsStaleToolOverrideSnapshot(t *testing.T) {
	db := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
	ctx := t.Context()
	preparePluginPolicyCutoverSchema(t, db)
	const registrationID = "0198f9a4-1b2c-7def-8123-456789abcdea"
	if _, err := db.Exec(ctx, `
		INSERT INTO mcp_server (id, scope, name, url, transport, auth_type, enabled,
			metadata, tools, status, credential_mode)
		VALUES ($1, 'system', 'github', 'https://mcp.example.test', 'sse', 'none',
			true, '{}'::jsonb, '[{"name":"create_issue"}]'::jsonb, 'ok', 'shared')
	`, registrationID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO tool_override (tool_name, scope, enabled)
		VALUES ('mcp__github__create_issue', 'system', false)
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		CREATE FUNCTION mutate_legacy_tool_override() RETURNS trigger
		LANGUAGE plpgsql AS $fn$
		BEGIN
			UPDATE tool_override SET enabled = true WHERE tool_name = 'mcp__github__create_issue';
			RETURN NEW;
		END;
		$fn$
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		CREATE TRIGGER mutate_legacy_tool_override_after_definition
			AFTER INSERT ON plugin_definition FOR EACH ROW
			EXECUTE FUNCTION mutate_legacy_tool_override()
	`); err != nil {
		t.Fatal(err)
	}
	if err := plugin.ImportLegacyState(ctx, db, plugin.NewCatalog(), nil); !errors.Is(err, plugin.ErrLegacyMigrationConflict) {
		t.Fatalf("stale tool override error = %v", err)
	}
	var enabled bool
	if err := db.QueryRow(ctx, `SELECT enabled FROM tool_override WHERE tool_name = 'mcp__github__create_issue'`).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("stale-row trigger mutation escaped importer rollback")
	}
	var definitions, configs, markers int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_definition`).Scan(&definitions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_config`).Scan(&configs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM app_setting WHERE key = 'plugin_cutover_v1'`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if definitions != 0 || configs != 0 || markers != 0 {
		t.Fatalf("stale-row rejection left target state: definitions=%d configs=%d markers=%d", definitions, configs, markers)
	}
}

func TestImportLegacyStateRollsBackWhenRetargetedOAuthValidationFindsOrphan(t *testing.T) {
	db, provider := newTestDBAtMigration(t, pluginCutoverMigration40)
	ctx := t.Context()
	user := insertPluginUser(t, db, "legacy-import-oauth-orphan@example.test", false)
	catalog := plugin.NewCatalog()
	if err := catalog.Register(pluginDefinition("builtin/import", "import", true)); err != nil {
		t.Fatal(err)
	}
	const mcpID = "0198f9a4-1b2c-7def-8123-456789abcded"
	const orphanID = "0198f9a4-1b2c-7def-8123-456789abcdee"
	if _, err := db.Exec(ctx, `
		INSERT INTO mcp_server (id, scope, name, url, transport, auth_type,
			enabled, metadata, tools, status, credential_mode)
		VALUES ($1, 'system', 'oauth-server', 'https://mcp.example.test', 'sse', 'none',
			true, '{}'::jsonb, '[]'::jsonb, 'unknown', 'shared')
	`, mcpID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO mcp_oauth_flow (
			server_id, user_id, credential_scope, pkce_verifier, oauth_config, expires_at
		) VALUES ($1, $2, 'system', 'valid', '{}'::jsonb, now() + interval '5 minutes')
	`, mcpID, user.UserID()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `ALTER TABLE mcp_oauth_flow DROP CONSTRAINT mcp_oauth_flow_server_id_fkey`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO mcp_oauth_flow (
			server_id, user_id, credential_scope, pkce_verifier, oauth_config, expires_at
		) VALUES ($1, $2, 'system', 'orphan', '{}'::jsonb, now() + interval '5 minutes')
	`, orphanID, user.UserID()); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, pluginCutoverMigration41); err != nil {
		t.Fatalf("apply embedded migration 41: %v", err)
	}
	preparePluginPolicyCutoverSchema(t, db)
	err := plugin.ImportLegacyState(ctx, db, catalog, nil)
	if err == nil || errors.Is(err, plugin.ErrOAuthForeignKeySchema) {
		t.Fatalf("orphan OAuth validation error = %v", err)
	}
	var definitions, configs, markers int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_definition`).Scan(&definitions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_config`).Scan(&configs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM app_setting WHERE key = 'plugin_cutover_v1'`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if definitions != 0 || configs != 0 || markers != 0 {
		t.Fatalf("failed OAuth validation left target state: definitions=%d configs=%d markers=%d", definitions, configs, markers)
	}
}
