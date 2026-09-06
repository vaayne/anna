package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/CherryHQ/stella/internal/mcp"
	"github.com/CherryHQ/stella/internal/platform/config"
	pluginpkg "github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/internal/server"
)

type pluginProbeFixture struct {
	kind     string
	name     string
	configID string
}

func setupPluginProbeHTTPEnv(t *testing.T) (*testEnv, *[]mcp.CredentialOwner) {
	t.Helper()
	env := setupAdmin(t)
	plugins := pluginpkg.NewService(env.db, env.deps.AgentAccess, pluginpkg.NewCatalog(), mcp.NewMCPBackendPolicy(mcp.EndpointPolicy{}),
		func(_ context.Context, fn func() error) error { return fn() })
	owners := []mcp.CredentialOwner{}
	mcpSvc := mcp.NewServiceForPool(env.db, oauthTestVault{}, func(pgx.Tx) mcp.Vault {
		return oauthTestVault{}
	})
	mcpSvc.SetPluginService(plugins)
	mcpSvc.SetConnectForTesting(func(_ context.Context, _ mcp.Registration, owner mcp.CredentialOwner) (mcp.RemoteClient, error) {
		owners = append(owners, owner)
		return &fakeRemote{}, nil
	})
	env.rebuild(t, func(d *server.Deps) {
		d.PluginService = plugins
		d.MCP = mcpSvc
		d.MCPAccess = mcp.NewAccess(mcpSvc, d.AgentAccess, nil)
	})
	return env, &owners
}

func installPluginProbeFixture(t *testing.T, env *testEnv, backend pluginpkg.Backend, scope, userID, agentID, authType, credentialMode string) pluginProbeFixture {
	t.Helper()
	const kind = "custom"
	name := "probe-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	pluginID := kind + "/" + name
	namespace := name
	configID := uuid.NewString()
	if _, err := env.db.Exec(context.Background(), `
		INSERT INTO plugin_definition(id, namespace, display_name, backend, source,
			implementation_key, spec, default_enabled, revision)
		VALUES ($1, $2, 'Probe test', $3, 'custom', $3, '{}'::jsonb, false, 1)`, pluginID, namespace, string(backend)); err != nil {
		t.Fatalf("seed plugin definition: %v", err)
	}
	payload := json.RawMessage(`{"url":"https://mcp.example.test","transport":"streamable_http","auth_type":"none","credential_mode":"shared"}`)
	refs := json.RawMessage(`{}`)
	if backend == pluginpkg.BackendMCP && authType == mcp.AuthTypeOAuth && credentialMode == mcp.CredentialModePerUser {
		payload = json.RawMessage(`{"url":"https://mcp.example.test","transport":"streamable_http","auth_type":"oauth","credential_mode":"per_user"}`)
		refs = fmt.Appendf(nil, `{"oauth_bundle":{"name":"MCP_OAUTH_%s","mode":"per_user","owner":"per_user"}}`, strings.ToUpper(strings.ReplaceAll(configID, "-", "_")))
	}
	enabled := true
	if backend != pluginpkg.BackendMCP {
		enabled = false
		payload = []byte(`{}`)
	}
	if _, err := env.db.Exec(context.Background(), `
		INSERT INTO plugin_config(id, plugin_id, namespace, scope, user_id, agent_id,
			enabled, config, credential_refs, revision)
		VALUES ($1::uuid, $2, $3, $4, NULLIF($5, '')::uuid, NULLIF($6, ''),
			$7, $8::jsonb, $9::jsonb, 1)`, configID, pluginID, namespace, scope, userID, agentID, enabled, payload, refs); err != nil {
		t.Fatalf("seed plugin config: %v", err)
	}
	return pluginProbeFixture{kind: kind, name: name, configID: configID}
}

func pluginProbePath(f pluginProbeFixture) string {
	return fmt.Sprintf("/api/plugins/%s/%s/configs/%s/probe", f.kind, f.name, f.configID)
}

func TestPluginProbeHTTPAuthorizesParentAndBackend(t *testing.T) {
	env, _ := setupPluginProbeHTTPEnv(t)
	mcpFixture := installPluginProbeFixture(t, env, pluginpkg.BackendMCP, "system", "", "", mcp.AuthTypeNone, mcp.CredentialModeShared)
	if rr := doUnauthRequest(t, env.srv, http.MethodPost, pluginProbePath(mcpFixture), nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated probe status = %d, want 401 (body: %s)", rr.Code, rr.Body.String())
	}
	wrongParent := fmt.Sprintf("/api/plugins/%s/wrong/configs/%s/probe", mcpFixture.kind, mcpFixture.configID)
	if rr := doRequest(t, env, http.MethodPost, wrongParent, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("wrong parent probe status = %d, want 404 (body: %s)", rr.Code, rr.Body.String())
	}
	nonMCP := installPluginProbeFixture(t, env, pluginpkg.BackendCLI, "system", "", "", "", "")
	if rr := doRequest(t, env, http.MethodPost, pluginProbePath(nonMCP), nil); rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "does not support probing") {
		t.Fatalf("non-MCP probe response = %d %s, want clear 400", rr.Code, rr.Body.String())
	}
}

func TestPluginProbeHTTPReturnsAgentPEPForbidden(t *testing.T) {
	env, _ := setupPluginProbeHTTPEnv(t)
	user, token := createTestUserWithToken(t, env.authStore, env.oidcStore, "probe-user", "user")
	agentID := "probe-private-agent"
	if err := env.store.CreateAgent(context.Background(), config.Agent{ID: agentID, Name: "Private", Model: "test/model", Scope: config.AgentScopeRestricted, CreatorID: "another-owner", Enabled: true}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	fixture := installPluginProbeFixture(t, env, pluginpkg.BackendMCP, mcp.ScopeUserAgent, user.ID, agentID, mcp.AuthTypeNone, mcp.CredentialModeShared)
	if rr := doRequestWithSession(t, env.srv, token, http.MethodPost, pluginProbePath(fixture), nil); rr.Code != http.StatusForbidden {
		t.Fatalf("unauthorized agent probe status = %d, want 403 (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestPluginProbeHTTPUsesOwnPerUserObservation(t *testing.T) {
	env, _ := setupPluginProbeHTTPEnv(t)
	userA, tokenA := createTestUserWithToken(t, env.authStore, env.oidcStore, "probe-owner", "user")
	userB, _ := createTestUserWithToken(t, env.authStore, env.oidcStore, "probe-other", "user")
	fixture := installPluginProbeFixture(t, env, pluginpkg.BackendMCP, mcp.ScopeUser, userA.ID, "", mcp.AuthTypeOAuth, mcp.CredentialModePerUser)
	if _, err := env.db.Exec(context.Background(), `
		INSERT INTO mcp_connection_state(config_id, credential_user_id, tools, status, status_error, config_revision)
		VALUES ($1::uuid, $2::uuid, '[]'::jsonb, 'error', 'other-user', 1)`, fixture.configID, userB.ID); err != nil {
		t.Fatalf("seed other-user observation: %v", err)
	}
	if rr := doRequestWithSession(t, env.srv, tokenA, http.MethodPost, pluginProbePath(fixture), nil); rr.Code != http.StatusOK {
		t.Fatalf("owner probe status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	} else if strings.Contains(rr.Body.String(), "mcp.example.test") {
		t.Fatalf("probe response exposed MCP endpoint: %s", rr.Body.String())
	}
	var ownerStatus, otherStatus, otherError string
	if err := env.db.QueryRow(context.Background(), `SELECT status FROM mcp_connection_state WHERE config_id = $1::uuid AND credential_user_id = $2::uuid`, fixture.configID, userA.ID).Scan(&ownerStatus); err != nil {
		t.Fatalf("read owner observation: %v", err)
	}
	if err := env.db.QueryRow(context.Background(), `SELECT status, status_error FROM mcp_connection_state WHERE config_id = $1::uuid AND credential_user_id = $2::uuid`, fixture.configID, userB.ID).Scan(&otherStatus, &otherError); err != nil {
		t.Fatalf("read other observation: %v", err)
	}
	if ownerStatus != mcp.StatusNeedsAuth {
		t.Fatalf("owner observation status = %q, want %q", ownerStatus, mcp.StatusNeedsAuth)
	}
	if otherStatus != mcp.StatusError || otherError != "other-user" {
		t.Fatalf("other observation changed to %q/%q", otherStatus, otherError)
	}
}
