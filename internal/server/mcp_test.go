package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/CherryHQ/stella/internal/agent"
	"github.com/CherryHQ/stella/internal/mcp"
	pluginpkg "github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/internal/server"
)

// fakeRemote is the canned remote MCP client behind mcp.Service's test-only
// connect hook. Real endpoints are unreachable in tests because the SSRF-safe
// dialer refuses loopback targets, so the transport seam is faked instead.
type fakeRemote struct {
	tools []*mcpsdk.Tool
}

func (c *fakeRemote) ListTools(context.Context) ([]*mcpsdk.Tool, error) {
	return c.tools, nil
}

func (c *fakeRemote) CallTool(context.Context, string, map[string]any) (*mcpsdk.CallToolResult, error) {
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil
}

func (c *fakeRemote) Close() error { return nil }

func TestLegacyMCPManagementRoutesRemoved(t *testing.T) {
	env, _ := setupPluginMutationHTTP(t)
	paths := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/mcp/servers"},
		{http.MethodPost, "/api/mcp/servers"},
		{http.MethodGet, "/api/mcp/servers/example"},
		{http.MethodPatch, "/api/mcp/servers/example"},
		{http.MethodDelete, "/api/mcp/servers/example"},
		{http.MethodPost, "/api/mcp/servers/example/probe"},
		{http.MethodPost, "/api/mcp/servers/example/oauth-start"},
		{http.MethodPost, "/api/mcp/servers/example/oauth-disconnect"},
	}
	for _, route := range paths {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			rr := doRequest(t, env, route.method, route.path, nil)
			want := http.StatusNotFound
			if route.method == http.MethodPost || route.method == http.MethodPatch || route.method == http.MethodDelete {
				want = http.StatusMethodNotAllowed
			}
			if rr.Code != want {
				t.Fatalf("legacy MCP route = %d, want %d (body: %s)", rr.Code, want, rr.Body.String())
			}
		})
	}
}

// seedCatalogedMCPServer inserts a user-scope common MCP configuration whose
// persisted observation lists one tool, so the profile tools endpoint can
// enumerate it without connecting anywhere.
func seedCatalogedMCPServer(t *testing.T, env *testEnv) {
	t.Helper()
	ctx := context.Background()
	const pluginID, namespace = "custom/gh", "github"
	configID := uuid.NewString()
	tools, err := json.Marshal([]mcp.CatalogTool{{
		Name: "create_issue", Description: "Create an issue.",
		InputSchema: map[string]any{"type": "object"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `
		INSERT INTO plugin_definition(id, namespace, display_name, backend, source,
			implementation_key, spec, default_enabled, revision)
		VALUES ($1, $2, 'GitHub', 'mcp', 'custom', 'mcp', '{}'::jsonb, false, 1)`, pluginID, namespace); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	if _, err := env.db.Exec(ctx, `
		INSERT INTO plugin_config(id, plugin_id, namespace, scope, user_id, enabled,
			config, credential_refs, revision)
		VALUES ($1::uuid, $2, $3, 'user', $4::uuid, true,
			$5::jsonb, '{}'::jsonb, 1)`, configID, pluginID, namespace, env.adminUser.ID,
		`{"url":"https://mcp.example.com","transport":"streamable_http","auth_type":"none"}`); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if _, err := env.db.Exec(ctx, `
		INSERT INTO mcp_connection_state(config_id, tools, status, probed_at, config_revision)
		VALUES ($1::uuid, $2::jsonb, 'ok', now(), 1)`, configID, tools); err != nil {
		t.Fatalf("seed observation: %v", err)
	}
}

func setupMCPCatalogEnv(t *testing.T) *testEnv {
	t.Helper()
	env := setupAdmin(t)
	seedCatalogedMCPServer(t, env)
	plugins := pluginpkg.NewService(env.db, env.deps.AgentAccess, pluginpkg.NewCatalog(),
		mcp.NewMCPBackendPolicy(mcp.EndpointPolicy{}),
		func(_ context.Context, fn func() error) error { return fn() })
	svc := mcp.NewServiceForPool(env.db, nil, nil)
	svc.SetPluginService(plugins)
	env.rebuild(t, func(d *server.Deps) {
		d.PluginService = plugins
		d.MCP = svc
		d.MCPAccess = mcp.NewAccess(svc, d.AgentAccess, nil)
	})
	return env
}

func TestAgentToolsListIncludesMCPCatalogEntries(t *testing.T) {
	env := setupMCPCatalogEnv(t)
	agentID := findStellaID(t, env)

	rr := doRequest(t, env, http.MethodGet, "/api/agents/"+agentID+"/tools", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (body: %s)", rr.Code, rr.Body.String())
	}
	var got struct {
		Tools []struct {
			Name    string `json:"name"`
			Source  string `json:"source"`
			Family  string `json:"family"`
			Control string `json:"control"`
			Enabled *bool  `json:"enabled"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode tools: %v", err)
	}
	var mcpTools []struct {
		Name    string `json:"name"`
		Source  string `json:"source"`
		Family  string `json:"family"`
		Control string `json:"control"`
		Enabled *bool  `json:"enabled"`
	}
	for _, tool := range got.Tools {
		if tool.Source == "mcp" {
			mcpTools = append(mcpTools, tool)
		}
	}
	if len(mcpTools) != 1 {
		t.Fatalf("mcp tools = %#v, want exactly the cataloged tool", mcpTools)
	}
	tool := mcpTools[0]
	if tool.Name != "github__create_issue" || tool.Family != "mcp:GitHub" || tool.Control != "override" {
		t.Fatalf("mcp tool = %#v", tool)
	}
	if tool.Enabled == nil || !*tool.Enabled {
		t.Fatalf("enabled = %v, want the default true", tool.Enabled)
	}
}

func TestAgentToolOverrideUsesUnifiedMCPIdentity(t *testing.T) {
	env := setupMCPCatalogEnv(t)
	agentID := findStellaID(t, env)
	const toolName = "github__create_issue"

	// The unified registration carries a trusted plugin/local identity, so a
	// user-agent override is accepted and is keyed by that identity.
	rr := doRequest(t, env, http.MethodPatch, "/api/agents/"+agentID+"/tools/"+toolName,
		map[string]any{"enabled": false, "scope": "user_agent"})
	if rr.Code != http.StatusOK {
		t.Fatalf("unified user_agent patch status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}

	rr = doRequest(t, env, http.MethodGet, "/api/agents/"+agentID+"/tools", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get tools status = %d", rr.Code)
	}
	var got struct {
		Tools []struct {
			Name    string `json:"name"`
			Enabled *bool  `json:"enabled"`
			Origin  string `json:"origin"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode tools: %v", err)
	}
	var tool struct {
		Name    string `json:"name"`
		Enabled *bool  `json:"enabled"`
		Origin  string `json:"origin"`
	}
	for _, item := range got.Tools {
		if item.Name == toolName {
			tool = item
		}
	}
	if tool.Name == "" || tool.Enabled == nil {
		t.Fatalf("tool %q missing from list", toolName)
	}
	if *tool.Enabled || tool.Origin != "user_agent" {
		t.Fatalf("unified MCP decision = enabled %v origin %q, want false/user_agent", *tool.Enabled, tool.Origin)
	}

	// Even a stale legacy row cannot be interpreted as a core identity by the
	// runtime MCP path, which has no trusted identity to match.
	overrides := []agent.ToolOverride{
		{Identity: agent.ToolIdentity{}, Scope: agent.ToolOverrideScopeSystemAgent, Enabled: false},
		{Identity: agent.ToolIdentity{}, Scope: agent.ToolOverrideScopeUserAgent, Enabled: true},
	}
	if !agent.FilterToolEnabled(true, agent.ToolIdentity{}, overrides) {
		t.Fatal("FilterToolEnabled applied an empty-identity override")
	}
}

func TestAgentToolOverrideRejectsUnknownMCPName(t *testing.T) {
	env := setupMCPCatalogEnv(t)
	agentID := findStellaID(t, env)

	rr := doRequest(t, env, http.MethodPatch, "/api/agents/"+agentID+"/tools/github__missing",
		map[string]any{"enabled": false, "scope": "user_agent"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown mcp tool status = %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestAgentToolsPerUserNeedsAuth proves a per_user registration without the
// calling user's bundle lists its tools with availability_reason
// mcp_needs_auth even though the row's status may be shared/ok.
func TestAgentToolsPerUserNeedsAuth(t *testing.T) {
	env := setupAdmin(t)
	ctx := context.Background()
	const pluginID, namespace = "custom/notion", "notion"
	configID := uuid.NewString()
	if _, err := env.db.Exec(ctx, `
		INSERT INTO plugin_definition(id, namespace, display_name, backend, source,
			implementation_key, spec, default_enabled, revision)
		VALUES ($1, $2, 'Notion', 'mcp', 'custom', 'mcp', '{}'::jsonb, false, 1)`, pluginID, namespace); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	payload := `{"url":"https://mcp.example.com","transport":"streamable_http","auth_type":"oauth","credential_mode":"per_user"}`
	refs := `{"oauth_bundle":{"name":"MCP_OAUTH_` + strings.ToUpper(strings.ReplaceAll(configID, "-", "_")) + `","mode":"per_user","owner":"per_user"}}`
	if _, err := env.db.Exec(ctx, `
		INSERT INTO plugin_config(id, plugin_id, namespace, scope, agent_id, enabled,
			config, credential_refs, revision)
		VALUES ($1::uuid, $2, $3, 'system_agent', 'stella', true, $4::jsonb, $5::jsonb, 1)`,
		configID, pluginID, namespace, payload, refs); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	tools, err := json.Marshal([]mcp.CatalogTool{{
		Name: "search", Description: "Search.",
		InputSchema: map[string]any{"type": "object"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `
		INSERT INTO mcp_connection_state(config_id, credential_user_id, tools, status, probed_at, config_revision)
		VALUES ($1::uuid, $2::uuid, $3::jsonb, 'ok', now(), 1)`, configID, env.adminUser.ID, tools); err != nil {
		t.Fatalf("seed observation: %v", err)
	}
	plugins := pluginpkg.NewService(env.db, env.deps.AgentAccess, pluginpkg.NewCatalog(),
		mcp.NewMCPBackendPolicy(mcp.EndpointPolicy{}),
		func(_ context.Context, fn func() error) error { return fn() })
	svc := mcp.NewServiceForPool(env.db, nil, nil)
	svc.SetPluginService(plugins)
	env.rebuild(t, func(d *server.Deps) {
		d.PluginService = plugins
		d.MCP = svc
		d.MCPAccess = mcp.NewAccess(svc, d.AgentAccess, nil)
	})

	rr := doRequest(t, env, http.MethodGet, "/api/agents/stella/tools", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (body: %s)", rr.Code, rr.Body.String())
	}
	var got struct {
		Tools []struct {
			Name               string  `json:"name"`
			Enabled            *bool   `json:"enabled"`
			AvailabilityReason *string `json:"availability_reason"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode tools: %v", err)
	}
	var tool struct {
		Name               string  `json:"name"`
		Enabled            *bool   `json:"enabled"`
		AvailabilityReason *string `json:"availability_reason"`
	}
	for _, item := range got.Tools {
		if item.Name == "notion__search" {
			tool = item
		}
	}
	if tool.Name == "" {
		t.Fatalf("per_user tool missing from the list: %#v", got.Tools)
	}
	if tool.AvailabilityReason == nil || *tool.AvailabilityReason != "mcp_needs_auth" {
		t.Fatalf("availability_reason = %v, want mcp_needs_auth", tool.AvailabilityReason)
	}
	if tool.Enabled == nil || !*tool.Enabled {
		t.Fatalf("enabled = %v, want the default true (override stays editable)", tool.Enabled)
	}
}
