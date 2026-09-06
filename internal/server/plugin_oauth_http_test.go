package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/CherryHQ/stella/internal/mcp"
	"github.com/CherryHQ/stella/internal/platform/config"
	pluginpkg "github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/internal/server"
)

// oauthTestVault is deliberately empty. The negative action cases stop at the
// MCP PEP before a credential is read or written; the per-user initialization
// case only needs bindVault to pass the metadata validation guard.
type oauthTestVault struct{}

func (oauthTestVault) SetScoped(context.Context, string, string, string, string, string) error {
	return nil
}

func (oauthTestVault) SetSystemScoped(context.Context, string, string, string, string) error {
	return nil
}

func (oauthTestVault) GetScoped(context.Context, string, string, string, string) (string, error) {
	return "", nil
}

func (oauthTestVault) DeleteScoped(context.Context, string, string, string, string) error {
	return nil
}

func (oauthTestVault) DeleteSystemScoped(context.Context, string, string, string) error {
	return nil
}

type pluginOAuthFixture struct {
	id   string
	kind string
	name string
}

func installPluginOAuthFixture(t *testing.T, env *testEnv, scope, userID, agentID, mode, endpoint string) pluginOAuthFixture {
	t.Helper()
	const kind, name, namespace = "custom", "oauth-test", "oauth-test"
	pluginID := kind + "/" + name
	configID := uuid.NewString()
	if _, err := env.db.Exec(context.Background(), `
		INSERT INTO plugin_definition(id, namespace, display_name, backend, source,
			implementation_key, spec, default_enabled, revision)
		VALUES ($1, $2, 'OAuth test', 'mcp', 'custom', 'mcp', '{}'::jsonb, false, 1)
		ON CONFLICT (id) DO NOTHING`, pluginID, namespace); err != nil {
		t.Fatalf("seed plugin definition: %v", err)
	}
	payload, err := json.Marshal(map[string]string{
		"url": endpoint, "transport": mcp.TransportStreamableHTTP,
		"auth_type": mcp.AuthTypeOAuth, "credential_mode": mode,
	})
	if err != nil {
		t.Fatalf("marshal OAuth payload: %v", err)
	}
	refs := map[string]any{"oauth_bundle": map[string]any{
		"name": "MCP_OAUTH_" + strings.ToUpper(strings.ReplaceAll(configID, "-", "_")),
		"mode": mode,
	}}
	if mode == mcp.CredentialModePerUser {
		refs["oauth_bundle"].(map[string]any)["owner"] = "per_user"
	} else {
		bundle := refs["oauth_bundle"].(map[string]any)
		bundle["scope"], bundle["user_id"], bundle["agent_id"] = scope, userID, agentID
	}
	credentialRefs, err := json.Marshal(refs)
	if err != nil {
		t.Fatalf("marshal OAuth refs: %v", err)
	}
	if _, err := env.db.Exec(context.Background(), `
		INSERT INTO plugin_config(id, plugin_id, namespace, scope, user_id, agent_id,
			enabled, config, credential_refs, revision)
		VALUES ($1::uuid, $2, $3, $4, NULLIF($5, '')::uuid, NULLIF($6, ''),
			true, $7::jsonb, $8::jsonb, 1)`, configID, pluginID, namespace, scope, userID, agentID, payload, credentialRefs); err != nil {
		t.Fatalf("seed plugin config: %v", err)
	}
	return pluginOAuthFixture{id: configID, kind: kind, name: name}
}

func setupPluginOAuthHTTPEnv(t *testing.T, endpointPolicy mcp.EndpointPolicy) *testEnv {
	t.Helper()
	env := setupAdmin(t)
	plugins := pluginpkg.NewService(env.db, env.deps.AgentAccess, pluginpkg.NewCatalog(), mcp.NewMCPBackendPolicy(endpointPolicy),
		func(_ context.Context, fn func() error) error { return fn() })
	mcpSvc := mcp.NewServiceForPool(env.db, oauthTestVault{}, func(pgx.Tx) mcp.Vault {
		return oauthTestVault{}
	})
	mcpSvc.SetEndpointPolicy(endpointPolicy)
	mcpSvc.SetPluginService(plugins)
	env.rebuild(t, func(d *server.Deps) {
		d.PluginService = plugins
		d.MCP = mcpSvc
		d.MCPAccess = mcp.NewAccess(mcpSvc, d.AgentAccess, nil)
	})
	return env
}

func TestPluginOAuthHTTPRejectsWrongParentAndUnknownConfig(t *testing.T) {
	env := setupPluginOAuthHTTPEnv(t, mcp.EndpointPolicy{})
	fixture := installPluginOAuthFixture(t, env, mcp.ScopeSystem, "", "", mcp.CredentialModeShared, "https://mcp.example.test/mcp")
	path := "/api/plugins/" + fixture.kind + "/" + fixture.name + "/configs/" + fixture.id + "/oauth/start"
	if rr := doUnauthRequest(t, env.srv, http.MethodPost, path, nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated start status = %d, want 401 (body: %s)", rr.Code, rr.Body.String())
	}

	for _, action := range []string{"start", "disconnect"} {
		wrongParent := "/api/plugins/custom/wrong/configs/" + fixture.id + "/oauth/" + action
		if rr := doRequest(t, env, http.MethodPost, wrongParent, nil); rr.Code != http.StatusNotFound {
			t.Fatalf("wrong parent %s status = %d, want 404 (body: %s)", action, rr.Code, rr.Body.String())
		}
		unknownConfig := "/api/plugins/" + fixture.kind + "/" + fixture.name + "/configs/" + uuid.NewString() + "/oauth/" + action
		if rr := doRequest(t, env, http.MethodPost, unknownConfig, nil); rr.Code != http.StatusNotFound {
			t.Fatalf("unknown config %s status = %d, want 404 (body: %s)", action, rr.Code, rr.Body.String())
		}
	}
}

func TestPluginOAuthHTTPRejectsUnauthorizedAgentAndSharedScope(t *testing.T) {
	env := setupPluginOAuthHTTPEnv(t, mcp.EndpointPolicy{})
	user, userToken := createTestUserWithToken(t, env.authStore, env.oidcStore, "oauth-user", "user")
	agentID := "oauth-private-agent"
	if err := env.store.CreateAgent(context.Background(), config.Agent{
		ID: agentID, Name: "OAuth private", Model: "test/model",
		Scope: config.AgentScopeRestricted, CreatorID: "different-owner", Enabled: true,
	}); err != nil {
		t.Fatalf("seed private agent: %v", err)
	}
	userAgent := installPluginOAuthFixture(t, env, mcp.ScopeUserAgent, user.ID, agentID, mcp.CredentialModeShared, "https://mcp.example.test/mcp")
	systemAgent := installPluginOAuthFixture(t, env, mcp.ScopeSystemAgent, "", agentID, mcp.CredentialModeShared, "https://mcp.example.test/mcp")
	systemShared := installPluginOAuthFixture(t, env, mcp.ScopeSystem, "", "", mcp.CredentialModeShared, "https://mcp.example.test/mcp")

	for _, test := range []struct {
		name    string
		fixture pluginOAuthFixture
	}{
		{name: "user-agent", fixture: userAgent},
		{name: "system-agent", fixture: systemAgent},
		{name: "shared-system", fixture: systemShared},
	} {
		for _, action := range []string{"start", "disconnect"} {
			path := "/api/plugins/" + test.fixture.kind + "/" + test.fixture.name + "/configs/" + test.fixture.id + "/oauth/" + action
			if rr := doRequestWithSession(t, env.srv, userToken, http.MethodPost, path, nil); rr.Code != http.StatusForbidden {
				t.Fatalf("unauthorized %s %s status = %d, want 403 (body: %s)", test.name, action, rr.Code, rr.Body.String())
			}
		}
	}
}

func TestPluginOAuthHTTPMapsSystemPerUserInitializationHint(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(remote.Close)
	env := setupPluginOAuthHTTPEnv(t, mcp.EndpointPolicy{AllowPrivate: true})
	_, userToken := createTestUserWithToken(t, env.authStore, env.oidcStore, "oauth-user", "user")
	fixture := installPluginOAuthFixture(t, env, mcp.ScopeSystem, "", "", mcp.CredentialModePerUser, remote.URL+"/mcp")
	path := "/api/plugins/" + fixture.kind + "/" + fixture.name + "/configs/" + fixture.id + "/oauth/start"
	rr := doRequestWithSession(t, env.srv, userToken, http.MethodPost, path, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("per-user initialization status = %d, want 409 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "administrator must initialize this connection before users can authorize their own accounts") {
		t.Fatalf("initialization hint missing from response: %s", rr.Body.String())
	}
}
