package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/jackc/pgx/v5"

	apitypes "github.com/CherryHQ/stella/api/types"
	"github.com/CherryHQ/stella/internal/mcp"
	"github.com/CherryHQ/stella/internal/platform/config"
	pluginpkg "github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/internal/server"
	"github.com/CherryHQ/stella/internal/vault"
)

func setupPluginMutationHTTP(t *testing.T) (*testEnv, *vault.Service) {
	t.Helper()
	env := setupAdmin(t)
	master, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := vault.NewServiceForPool(env.db, master.String(), env.deps.AgentAccess)
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := vault.GenerateUserKeys(secrets.MasterRecipient())
	if err != nil {
		t.Fatal(err)
	}
	if err := env.oidcStore.UpdateUserAgeKeys(t.Context(), env.adminUser.ID, public, private); err != nil {
		t.Fatal(err)
	}
	policy := mcp.EndpointPolicy{}
	plugins := pluginpkg.NewService(env.db, env.deps.AgentAccess, pluginpkg.NewCatalog(), mcp.NewMCPBackendPolicy(policy), func(_ context.Context, fn func() error) error { return fn() })
	backend := mcp.NewServiceForPool(env.db, secrets, func(tx pgx.Tx) mcp.Vault { return secrets.WithTx(tx) })
	backend.SetEndpointPolicy(policy)
	backend.SetPluginService(plugins)
	env.rebuild(t, func(d *server.Deps) {
		d.PluginService = plugins
		d.MCP = backend
		d.MCPAccess = mcp.NewAccess(backend, d.AgentAccess, nil)
	})
	return env, secrets
}

func TestPluginMCPHTTPAtomicCredentialsAndClosedProjection(t *testing.T) {
	env, secrets := setupPluginMutationHTTP(t)
	body := map[string]any{
		"namespace": "http_mcp", "display_name": "HTTP MCP", "backend": "mcp", "definition_spec": map[string]any{},
		"initial_config": map[string]any{"scope": "user", "is_enabled": true, "config": map[string]any{"url": "https://mcp.example.test/path/endpoint-secret", "auth_type": "bearer", "transport": "streamable_http"}, "credentials": map[string]any{"token": "first-bearer-secret"}},
	}
	if rr := doUnauthRequest(t, env.srv, http.MethodPost, "/api/plugins", body); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status=%d", rr.Code)
	}
	rr := doRequest(t, env, http.MethodPost, "/api/plugins", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", rr.Code, rr.Body.String())
	}
	for _, value := range []string{"first-bearer-secret", "endpoint-secret", "https://mcp.example.test", "credential_refs"} {
		if strings.Contains(rr.Body.String(), value) {
			t.Fatalf("response exposed %q", value)
		}
	}
	var created apitypes.CreatePluginResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	cfg := created.Config
	name := "MCP_TOKEN_" + strings.ToUpper(strings.ReplaceAll(cfg.Id.String(), "-", "_"))
	if got, err := secrets.GetScoped(t.Context(), "user", env.adminUser.ID, "", name); err != nil || got != "first-bearer-secret" {
		t.Fatalf("stored initial credential: %v", err)
	}
	for _, mode := range []string{"false", "null", "omitted"} {
		agentID := "mcp-create-" + mode
		if err := env.store.CreateAgent(t.Context(), config.Agent{ID: agentID, Name: agentID, Model: "test/model", Scope: config.AgentScopeSystem, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		child := map[string]any{"scope": "system_agent", "agent_id": agentID, "config": map[string]any{"url": "https://mcp.example.test", "auth_type": "none", "transport": "streamable_http"}}
		if mode == "false" {
			child["is_enabled"] = false
		}
		if mode == "null" {
			child["is_enabled"] = nil
		}
		response := doRequest(t, env, http.MethodPost, "/api/plugins/"+created.Plugin.Id+"/configs", child)
		if response.Code != http.StatusCreated {
			t.Fatalf("child %s=%d %s", mode, response.Code, response.Body.String())
		}
		var nullValue, falseValue bool
		if err := env.db.QueryRow(t.Context(), `SELECT enabled IS NULL, enabled IS FALSE FROM plugin_config WHERE plugin_id=$1 AND scope='system_agent' AND agent_id=$2`, created.Plugin.Id, agentID).Scan(&nullValue, &falseValue); err != nil {
			t.Fatal(err)
		}
		if mode == "false" && !falseValue || mode != "false" && !nullValue {
			t.Fatalf("child %s null=%v false=%v", mode, nullValue, falseValue)
		}
	}
	for _, scope := range []string{"system_agent", "user_agent"} {
		invalid := map[string]any{"scope": scope, "config": map[string]any{"url": "https://mcp.example.test", "auth_type": "none", "transport": "streamable_http"}}
		response := doRequest(t, env, http.MethodPost, "/api/plugins/"+created.Plugin.Id+"/configs", invalid)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("missing agent %s=%d %s", scope, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "mcp.example.test") {
			t.Fatal("owner validation exposed config")
		}
	}
	path := "/api/plugins/" + created.Plugin.Id + "/configs/" + cfg.Id.String()
	var revision int64
	if err := env.db.QueryRow(t.Context(), `SELECT revision FROM plugin_config WHERE id=$1`, cfg.Id.String()).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	update := map[string]any{"expected_revision": revision, "config": map[string]any{"url": "https://changed.example.test/mcp"}, "credentials": map[string]any{"token": "second-bearer-secret"}}
	wrong := "/api/plugins/custom/wrong/configs/" + cfg.Id.String()
	if rr := doRequest(t, env, http.MethodPatch, wrong, update); rr.Code != http.StatusNotFound {
		t.Fatalf("wrong parent=%d %s", rr.Code, rr.Body.String())
	}
	invalid := map[string]any{"expected_revision": revision, "config": map[string]any{"url": "https://changed.example.test/mcp"}}
	rejected := doRequest(t, env, http.MethodPatch, path, invalid)
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("missing replacement=%d %s", rejected.Code, rejected.Body.String())
	}
	if strings.Contains(rejected.Body.String(), "changed.example.test") || strings.Contains(rejected.Body.String(), "first-bearer-secret") {
		t.Fatal("validation response leaked connection")
	}
	rr = doRequest(t, env, http.MethodPatch, path, update)
	if rr.Code != http.StatusOK {
		t.Fatalf("replace=%d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "second-bearer-secret") {
		t.Fatal("replacement leaked")
	}
	if got, err := secrets.GetScoped(t.Context(), "user", env.adminUser.ID, "", name); err != nil || got != "second-bearer-secret" {
		t.Fatalf("replacement credential: %v", err)
	}
	if rr := doRequest(t, env, http.MethodPatch, path, update); rr.Code != http.StatusConflict {
		t.Fatalf("stale=%d %s", rr.Code, rr.Body.String())
	}
	if err := env.db.QueryRow(t.Context(), `SELECT revision FROM plugin_config WHERE id=$1`, cfg.Id.String()).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	rr = doRequest(t, env, http.MethodPatch, path, map[string]any{"expected_revision": revision, "is_enabled": nil})
	if rr.Code != http.StatusOK {
		t.Fatalf("inherit=%d %s", rr.Code, rr.Body.String())
	}
	var inherited bool
	if err := env.db.QueryRow(t.Context(), `SELECT enabled IS NULL FROM plugin_config WHERE id=$1`, cfg.Id.String()).Scan(&inherited); err != nil {
		t.Fatal(err)
	}
	if !inherited {
		t.Fatal("null enabled did not restore inheritance")
	}
	if got, err := secrets.GetScoped(t.Context(), "user", env.adminUser.ID, "", name); err != nil || got != "second-bearer-secret" {
		t.Fatalf("enable reset changed credential: %v", err)
	}
}

func TestPluginMCPHTTPRejectsRawLocatorsAndForeignScope(t *testing.T) {
	env, _ := setupPluginMutationHTTP(t)
	_, token := createTestUserWithToken(t, env.authStore, env.oidcStore, "plugin-http-user", "user")
	body := map[string]any{"namespace": "forbidden_mcp", "display_name": "Forbidden MCP", "backend": "mcp", "definition_spec": map[string]any{}, "initial_config": map[string]any{"scope": "system", "config": map[string]any{"url": "https://mcp.example.test", "auth_type": "none", "transport": "streamable_http"}}}
	rr := doRequestWithSession(t, env.srv, token, http.MethodPost, "/api/plugins", body)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("foreign scope=%d %s", rr.Code, rr.Body.String())
	}
	body["initial_config"].(map[string]any)["credential_refs"] = map[string]any{"bearer": map[string]any{"name": "foreign"}}
	rr = doRequest(t, env, http.MethodPost, "/api/plugins", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("raw locator=%d %s", rr.Code, rr.Body.String())
	}
	var rows int
	if err := env.db.QueryRow(t.Context(), `SELECT count(*) FROM plugin_definition WHERE namespace='forbidden_mcp'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatal("rejected creation persisted definition")
	}
}

func TestPluginMCPHTTPInvalidBackendInput(t *testing.T) {
	env, _ := setupPluginMutationHTTP(t)
	for _, field := range []string{"transport", "auth_type", "credential_mode"} {
		t.Run(field, func(t *testing.T) {
			cfg := map[string]any{"url": "https://mcp.example.test/private-path", "auth_type": "none", "transport": "streamable_http"}
			cfg[field] = "invalid-secret-value"
			body := map[string]any{"namespace": "invalid_mcp", "display_name": "Invalid MCP", "backend": "mcp", "definition_spec": map[string]any{}, "initial_config": map[string]any{"scope": "user", "is_enabled": true, "config": cfg}}
			rr := doRequest(t, env, http.MethodPost, "/api/plugins", body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			for _, secret := range []string{"private-path", "invalid-secret-value"} {
				if strings.Contains(rr.Body.String(), secret) {
					t.Fatal("error exposed input")
				}
			}
		})
	}
}

func TestPluginHTTPDefinitionLifecycleAndConfigIsolation(t *testing.T) {
	env, secrets := setupPluginMutationHTTP(t)
	create := func(namespace, displayName, endpoint string) apitypes.CreatePluginResponse {
		t.Helper()
		rr := doRequest(t, env, http.MethodPost, "/api/plugins", map[string]any{
			"namespace": namespace, "display_name": displayName, "backend": "mcp",
			"definition_spec": map[string]any{},
			"initial_config": map[string]any{
				"scope": "user", "is_enabled": true,
				"config":      map[string]any{"url": endpoint, "auth_type": "bearer", "transport": "streamable_http"},
				"credentials": map[string]any{"token": namespace + "-token"},
			},
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create %s = %d: %s", namespace, rr.Code, rr.Body.String())
		}
		var created apitypes.CreatePluginResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode create %s: %v", namespace, err)
		}
		if created.Plugin.Namespace != namespace || created.Plugin.DisplayName != displayName {
			t.Fatalf("created plugin = %#v", created.Plugin)
		}
		return created
	}

	first := create("lifecycle_a", "Lifecycle A", "https://a.example.test/mcp")
	second := create("lifecycle_b", "Lifecycle B", "https://b.example.test/mcp")
	var before []byte
	if err := env.db.QueryRow(t.Context(), `SELECT config FROM plugin_config WHERE id = $1`, second.Config.Id.String()).Scan(&before); err != nil {
		t.Fatalf("read second config: %v", err)
	}
	updatePath := "/api/plugins/" + first.Plugin.Id + "/configs/" + first.Config.Id.String()
	update := map[string]any{
		"expected_revision": first.Config.Revision,
		"config":            map[string]any{"url": "https://a-updated.example.test/mcp", "auth_type": "bearer", "transport": "streamable_http"},
		"credentials":       map[string]any{"token": "lifecycle_a-replacement"},
	}
	rr := doRequest(t, env, http.MethodPatch, updatePath, update)
	if rr.Code != http.StatusOK {
		t.Fatalf("update first = %d: %s", rr.Code, rr.Body.String())
	}
	var after []byte
	if err := env.db.QueryRow(t.Context(), `SELECT config FROM plugin_config WHERE id = $1`, second.Config.Id.String()).Scan(&after); err != nil {
		t.Fatalf("read second config after first update: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("updating one plugin changed another config: before=%s after=%s", before, after)
	}

	catalog := pluginpkg.NewCatalog()
	if err := catalog.Register(pluginpkg.Definition{
		ID: "builtin/lifecycle", Namespace: "lifecycle_builtin", DisplayName: "Builtin lifecycle",
		Backend: pluginpkg.BackendMCP, Source: pluginpkg.SourceBuiltin, ImplementationKey: "lifecycle",
		Spec: json.RawMessage(`{}`), DefaultEnabled: true, Revision: 1,
	}); err != nil {
		t.Fatalf("register builtin: %v", err)
	}
	plugins := pluginpkg.NewService(env.db, env.deps.AgentAccess, catalog, mcp.NewMCPBackendPolicy(mcp.EndpointPolicy{}), func(_ context.Context, fn func() error) error { return fn() })
	backend := mcp.NewServiceForPool(env.db, secrets, func(tx pgx.Tx) mcp.Vault { return secrets.WithTx(tx) })
	backend.SetPluginService(plugins)
	env.rebuild(t, func(d *server.Deps) {
		d.PluginService = plugins
		d.MCP = backend
		d.MCPAccess = mcp.NewAccess(backend, d.AgentAccess, nil)
	})
	if err := plugins.SyncBuiltinDefaults(t.Context()); err != nil {
		t.Fatalf("sync builtin: %v", err)
	}
	builtinDelete := doRequest(t, env, http.MethodDelete, "/api/plugins/builtin/lifecycle?expected_revision=1", nil)
	if builtinDelete.Code != http.StatusForbidden {
		t.Fatalf("builtin delete = %d, want 403: %s", builtinDelete.Code, builtinDelete.Body.String())
	}
	var builtinCount int
	if err := env.db.QueryRow(t.Context(), `SELECT count(*) FROM plugin_definition WHERE id = 'builtin/lifecycle'`).Scan(&builtinCount); err != nil {
		t.Fatal(err)
	}
	if builtinCount != 1 {
		t.Fatal("builtin definition was deleted")
	}

	var secondRevision int64
	if err := env.db.QueryRow(t.Context(), `SELECT revision FROM plugin_config WHERE id = $1`, second.Config.Id.String()).Scan(&secondRevision); err != nil {
		t.Fatalf("read second revision: %v", err)
	}
	staleDelete := doRequest(t, env, http.MethodDelete, "/api/plugins/"+second.Plugin.Id+"?expected_revision=2", nil)
	if staleDelete.Code != http.StatusConflict {
		t.Fatalf("stale custom delete = %d, want 409: %s", staleDelete.Code, staleDelete.Body.String())
	}
	deleteConfig := doRequest(t, env, http.MethodDelete, "/api/plugins/"+second.Plugin.Id+"/configs/"+second.Config.Id.String()+"?expected_revision="+strconv.FormatInt(secondRevision, 10), nil)
	if deleteConfig.Code != http.StatusNoContent {
		t.Fatalf("custom config delete = %d, want 204: %s", deleteConfig.Code, deleteConfig.Body.String())
	}
	deleteSecond := doRequest(t, env, http.MethodDelete, "/api/plugins/"+second.Plugin.Id+"?expected_revision=1", nil)
	if deleteSecond.Code != http.StatusNoContent {
		t.Fatalf("custom delete = %d, want 204: %s", deleteSecond.Code, deleteSecond.Body.String())
	}
}
