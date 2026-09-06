package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	agentruntime "github.com/CherryHQ/stella/internal/agent/runtime"
	"github.com/CherryHQ/stella/internal/agent/sandbox"
	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/db/dbtest"
	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/pkg/ai"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	"github.com/CherryHQ/stella/pkg/tools"
)

// TestMigratedMCPOverrideReachesRunnerDeny exercises the cutover at the
// runtime boundary. The policy starts as a legacy exported name, is converted
// in the importer, read through the real ToolOverrideStore, and finally hides
// the MCP proxy in the runner registry.
func TestMigratedMCPOverrideReachesRunnerDeny(t *testing.T) {
	db := dbtest.NewAtMigration(t, runnerImportMigration41)
	ctx := t.Context()

	userID := uuid.NewString()
	agentID := "mcp-import-runner-agent"
	registrationID := uuid.NewString()
	seedRunnerIdentity(t, db, userID, agentID)
	if _, err := db.Exec(ctx, `
		INSERT INTO mcp_server (id, scope, name, url, transport, auth_type,
			enabled, metadata, tools, status, status_error, credential_mode, probed_at)
		VALUES ($1, 'system', 'remote', 'https://mcp.example.test', 'sse', 'none',
			true, '{}'::jsonb,
			'[{"name":"list","description":"list","inputSchema":{"type":"object"}}]'::jsonb,
			'ok', '', 'shared', now())
	`, registrationID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO tool_override (id, tool_name, scope, user_id, agent_id, enabled)
		VALUES ($1, 'mcp__remote__list', 'user_agent', $2, $3, false)
	`, uuid.NewString(), userID, agentID); err != nil {
		t.Fatal(err)
	}

	if err := plugin.ImportLegacyState(ctx, db, plugin.NewCatalog(), nil); err != nil {
		t.Fatalf("import legacy MCP state: %v", err)
	}
	// Import preserves the catalog and status but intentionally leaves the
	// observation timestamp cold. Mark this fixture as freshly probed so the
	// provider exercises the cached catalog and never dials the test endpoint.
	if _, err := db.Exec(ctx, `UPDATE mcp_connection_state SET probed_at = now() WHERE config_id = $1::uuid`, registrationID); err != nil {
		t.Fatal(err)
	}

	store := NewToolOverrideStore(db)
	overrides, err := store.Fetch(ctx, userID, agentID)
	if err != nil {
		t.Fatalf("fetch migrated tool override: %v", err)
	}
	wantIdentity := ToolIdentity{PluginID: "custom/" + registrationID, LocalToolName: "list"}
	if len(overrides) != 1 || overrides[0].Identity != wantIdentity || overrides[0].Scope != ToolOverrideScopeUserAgent || overrides[0].Enabled {
		t.Fatalf("migrated overrides = %+v, want disabled %v in user_agent", overrides, wantIdentity)
	}

	authority, err := authz.NewUserAuthority(authz.UserID(userID), false)
	if err != nil {
		t.Fatal(err)
	}
	plugins := plugin.NewService(db, nil, plugin.NewCatalog(), plugin.BackendPolicy{Transition: noopBackendTransition}, noOpMutationFence)
	snapshot, err := plugins.ResolveSnapshot(ctx, authority, "")
	if err != nil {
		t.Fatalf("resolve plugin snapshot: %v", err)
	}
	home := t.TempDir()
	view := pkgplugins.SessionPluginView{
		RegisteredPluginIDs: []string{"custom/" + registrationID},
		ExposedPluginIDs:    []string{"custom/" + registrationID},
	}
	runnerPlugins := agentruntime.NewPluginContext(snapshot, view)
	registry, _, _, err := buildToolRegistry(ctx, runnerConfig{
		Sandbox: sandbox.Config{Paths: sandbox.Paths{
			StellaHome: home,
			AgentRoot:  filepath.Join(home, "agents", agentID),
			UserRoot:   filepath.Join(home, "users", userID),
		}},
		BuiltinParams:       RunnerParams{UserID: userID, AgentID: agentID},
		PluginContext:       runnerPlugins,
		MCPToolProvider:     migratedMCPToolProvider{pluginID: "custom/" + registrationID},
		ToolOverrideFetcher: store.Fetch,
		SkillRevisionReader: emptySkillRuntime{},
		SkillReadAuthorizer: allowSkillReads{},
	}, &fakeSession{alive: true}, nil, ai.Model{}, "")
	if err != nil {
		t.Fatalf("build runner tool registry: %v", err)
	}
	defer func() { _ = registry.Close() }()
	if registry.Has("remote__list") {
		t.Fatal("migrated user_agent deny did not hide the MCP tool")
	}
}

// TestMigratedMCPNamespaceDenyDoesNotFallThrough proves that a more-specific
// disabled definition wins its namespace over an enabled system definition.
// The system definition is the imported registration; the user definition is
// an additional target row, so this remains a post-import runtime test.
func TestMigratedMCPNamespaceDenyDoesNotFallThrough(t *testing.T) {
	db := dbtest.NewAtMigration(t, runnerImportMigration41)
	ctx := t.Context()

	userID := uuid.NewString()
	registrationID := uuid.NewString()
	seedRunnerIdentity(t, db, userID, "namespace-agent")
	if _, err := db.Exec(ctx, `
		INSERT INTO mcp_server (id, scope, name, url, transport, auth_type,
			enabled, metadata, tools, status, status_error, credential_mode)
		VALUES ($1, 'system', 'remote', 'https://mcp.example.test', 'sse', 'none',
			true, '{}'::jsonb, '[{"name":"list"}]'::jsonb, 'ok', '', 'shared')
	`, registrationID); err != nil {
		t.Fatal(err)
	}
	if err := plugin.ImportLegacyState(ctx, db, plugin.NewCatalog(), nil); err != nil {
		t.Fatalf("import legacy MCP state: %v", err)
	}

	deniedID := uuid.NewString()
	deniedPluginID := "custom/" + deniedID
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin_definition (id, namespace, display_name, backend, source,
			implementation_key, spec, default_enabled, revision, creator_user_id)
		VALUES ($1, 'remote', 'user deny', 'mcp', 'custom', 'mcp', '{}'::jsonb, false, 1, $2)
	`, deniedPluginID, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO plugin_config (id, plugin_id, namespace, scope, user_id, enabled,
			config, credential_refs, revision)
		VALUES ($1, $2, 'remote', 'user', $3, false,
			'{"url":"https://mcp.example.test","transport":"sse","auth_type":"none","credential_mode":"shared","metadata":{}}'::jsonb,
			'{}'::jsonb, 1)
	`, deniedID, deniedPluginID, userID); err != nil {
		t.Fatal(err)
	}

	authority, err := authz.NewUserAuthority(authz.UserID(userID), false)
	if err != nil {
		t.Fatal(err)
	}
	plugins := plugin.NewService(db, nil, plugin.NewCatalog(), plugin.BackendPolicy{Transition: noopBackendTransition}, noOpMutationFence)
	snapshot, err := plugins.ResolveSnapshot(ctx, authority, "")
	if err != nil {
		t.Fatalf("resolve plugin snapshot: %v", err)
	}
	winner, err := snapshot.ResolveNamespace("remote")
	if err != nil {
		t.Fatalf("resolve remote namespace: %v", err)
	}
	if winner.PluginID != deniedPluginID || winner.IsEffectivelyEnabled {
		t.Fatalf("namespace winner = %+v, want disabled user definition %s", winner, deniedPluginID)
	}

	home := t.TempDir()
	view := pkgplugins.SessionPluginView{
		RegisteredPluginIDs: []string{"custom/" + registrationID, deniedPluginID},
		ExposedPluginIDs:    []string{"custom/" + registrationID, deniedPluginID},
	}
	_, _, _, err = buildToolRegistry(ctx, runnerConfig{
		Sandbox: sandbox.Config{Paths: sandbox.Paths{
			StellaHome: home,
			AgentRoot:  filepath.Join(home, "agents", "namespace-agent"),
			UserRoot:   filepath.Join(home, "users", userID),
		}},
		BuiltinParams:       RunnerParams{UserID: userID, AgentID: "namespace-agent"},
		PluginContext:       agentruntime.NewPluginContext(snapshot, view),
		MCPToolProvider:     migratedMCPToolProvider{pluginID: "custom/" + registrationID},
		SkillRevisionReader: emptySkillRuntime{},
		SkillReadAuthorizer: allowSkillReads{},
	}, &fakeSession{alive: true}, nil, ai.Model{}, "")
	if err == nil {
		t.Fatal("runner accepted a tool from the shadowed enabled definition")
	}
	if !strings.Contains(err.Error(), "not owned by the enabled namespace winner") {
		t.Fatalf("shadowed namespace error = %v", err)
	}
}

const runnerImportMigration41 = int64(90000000000041)

func noOpMutationFence(_ context.Context, fn func() error) error { return fn() }

func noopBackendTransition(context.Context, pgx.Tx, authz.Authority, plugin.MutationKind, plugin.Definition, *plugin.Config, *plugin.Config) error {
	return nil
}

type migratedMCPToolProvider struct{ pluginID string }

func (p migratedMCPToolProvider) ToolsForSnapshot(context.Context, plugin.Snapshot) ([]tools.Tool, error) {
	return []tools.Tool{migratedMCPTool{staticTool{name: "remote__list"}, p.pluginID, "list"}}, nil
}

type migratedMCPTool struct {
	staticTool
	pluginID string
	local    string
}

func (t migratedMCPTool) PluginToolIdentity() (string, string, bool) {
	return t.pluginID, t.local, true
}

func seedRunnerIdentity(t *testing.T, db *pgxpool.Pool, userID, agentID string) {
	t.Helper()
	ctx := t.Context()
	if _, err := db.Exec(ctx, `INSERT INTO auth_user (id, email) VALUES ($1, $2)`, userID, userID+"@test.invalid"); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlc.New(db).CreateAgent(ctx, sqlc.CreateAgentParams{
		ID: agentID, Name: agentID, Workspace: "/tmp/" + agentID,
		Sandbox: json.RawMessage(`{}`), Scope: "system", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
}
