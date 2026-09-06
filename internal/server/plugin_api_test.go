package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apiserver "github.com/CherryHQ/stella/api/server"
	"github.com/CherryHQ/stella/internal/auth"
	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/mcp"
	pluginpkg "github.com/CherryHQ/stella/internal/plugin"
)

func TestWritePluginErrorMapsOAuthClientInitialization(t *testing.T) {
	recorder := httptest.NewRecorder()
	writePluginError(recorder, mcp.ErrOAuthClientInitializationRequired)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("OAuth client initialization status = %d, want %d", recorder.Code, http.StatusConflict)
	}
	var body struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if body.Error.Code != http.StatusConflict {
		t.Fatalf("error code = %d, want %d", body.Error.Code, http.StatusConflict)
	}
	const want = "administrator must initialize this connection before users can authorize their own accounts"
	if body.Error.Message != want {
		t.Fatalf("error message = %q, want %q", body.Error.Message, want)
	}
}

func TestPluginMCPRegistrationViewDoesNotEchoEndpoint(t *testing.T) {
	reg := mcp.Registration{
		ID: "0190b2c2-6f8e-7c62-9f7e-ff9f7d0c7a11", PluginID: "custom/github",
		Scope: mcp.ScopeUser, UserID: "0190b2c2-6f8e-7c62-9f9f-7d0c7a110001",
		Name: "GitHub", URL: "https://user:secret@private.example/path?token=secret",
		Transport: mcp.TransportStreamableHTTP, AuthType: mcp.AuthTypeOAuth,
		CredentialMode: mcp.CredentialModePerUser, OAuthClientID: "public-client",
		OAuthClientSecretRef: "MCP_OAUTH_CLIENT_PRIVATE", ConfigRevision: 4,
		CreatedAt: time.Date(2026, 9, 6, 0, 0, 0, 0, time.FixedZone("CST", 8*60*60)),
		UpdatedAt: time.Date(2026, 9, 6, 0, 0, 1, 0, time.FixedZone("CST", 8*60*60)),
	}
	view, err := pluginMCPRegistrationView(reg)
	if err != nil {
		t.Fatalf("pluginMCPRegistrationView: %v", err)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	for _, forbidden := range []string{"private.example", "user:secret", "public-client", "MCP_OAUTH_CLIENT_PRIVATE"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("OAuth response exposed %q: %s", forbidden, encoded)
		}
	}
	if !strings.Contains(string(encoded), `"oauth_client_id_configured":true`) {
		t.Fatalf("OAuth summary omitted client state: %s", encoded)
	}
}

func TestPluginAccessAuthenticationPrecedesUnavailableService(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest(http.MethodGet, "/api/plugins", nil)
	unauthenticated := httptest.NewRecorder()
	server.ListPlugins(unauthenticated, request, apiserver.ListPluginsParams{})
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", unauthenticated.Code, http.StatusUnauthorized)
	}

	authenticatedRequest := request.WithContext(withAuthInfo(request.Context(), &AuthInfo{
		UserID: "user-1", Role: auth.RoleUser,
	}))
	serviceUnavailable := httptest.NewRecorder()
	server.ListPlugins(serviceUnavailable, authenticatedRequest, apiserver.ListPluginsParams{})
	if serviceUnavailable.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable service status = %d, want %d", serviceUnavailable.Code, http.StatusServiceUnavailable)
	}
}

func TestPluginDefinitionViewProjectsOnlySafeSummary(t *testing.T) {
	definition := pluginpkg.Definition{
		ID: "custom/plugin", Namespace: "plugin", DisplayName: "Plugin",
		Backend: pluginpkg.BackendMCP, Source: pluginpkg.SourceCustom, Revision: 1,
		Spec: json.RawMessage(`{"description":"safe","category":"utility","capabilities":["read"],"url":"https://private.example/path?token=secret","credential_refs":{"token":"vault://secret"}}`),
	}
	view, err := pluginDefinitionView(definition)
	if err != nil {
		t.Fatalf("pluginDefinitionView: %v", err)
	}
	if view.Spec["description"] != "safe" || view.Spec["category"] != "utility" {
		t.Fatalf("safe summary = %#v", view.Spec)
	}
	if _, ok := view.Spec["url"]; ok {
		t.Fatal("definition view exposed private url")
	}
	if _, ok := view.Spec["credential_refs"]; ok {
		t.Fatal("definition view exposed credential refs")
	}
}

func TestPluginConfigViewProjectsTypedSummary(t *testing.T) {
	enabled := true
	config := pluginpkg.Config{
		ID: "0190b2c2-6f8e-7c62-9f7e-ff9f7d0c7a11", PluginID: "custom/plugin",
		Scope: pluginpkg.ScopeUser, Enabled: &enabled,
		Payload:        json.RawMessage(`{"url":"https://user:secret@private.example/path?token=secret#fragment","transport":"sse","auth_type":"bearer"}`),
		CredentialRefs: json.RawMessage(`{"bearer":{"name":"vault-secret"}}`), Revision: 1,
	}
	definition := pluginpkg.Definition{ID: "custom/plugin", Backend: pluginpkg.BackendMCP, Spec: json.RawMessage(`{}`)}
	view, err := pluginConfigView(definition, config)
	if err != nil {
		t.Fatalf("pluginConfigView: %v", err)
	}
	mcpSummary, err := view.BackendSummary.AsPluginMCPBackendSummary()
	if err != nil {
		t.Fatalf("decode backend summary: %v", err)
	}
	if !mcpSummary.EndpointConfigured || !mcpSummary.BearerConfigured {
		t.Fatalf("MCP summary flags = %#v, want endpoint and bearer configured", mcpSummary)
	}
	encoded, err := json.Marshal(view.BackendSummary)
	if err != nil {
		t.Fatalf("marshal backend summary: %v", err)
	}
	for _, forbidden := range []string{"private.example", "vault://", "user:secret"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("backend summary exposed private payload %q: %s", forbidden, encoded)
		}
	}
}

func TestPluginMCPBackendSummaryProjectsOnlyConfigurationFlags(t *testing.T) {
	definition := json.RawMessage(`{
		"url":"https://user:definition-secret@private.example/definition/path?token=definition-secret#fragment",
		"transport":"streamable_http",
		"auth_type":"oauth",
		"credential_mode":"per_user",
		"metadata":{"oauth":{"client_id":"public-client-id"},"private":"metadata-secret"},
		"credential_refs":{"oauth_bundle":"vault://oauth-bundle","oauth_client_secret":"vault://oauth-secret"}
	}`)
	config := json.RawMessage(`{
		"url":"https://user:config-secret@private.example/config/path?token=config-secret#fragment",
		"metadata":{"oauth":{"client_id":"config-client-id"}},
		"credential_refs":{"bearer":"vault://bearer","oauth_bundle":"vault://config-bundle","oauth_client_secret":"vault://config-secret"}
	}`)
	refs := json.RawMessage(`{
		"bearer":"vault://bearer",
		"oauth_bundle":"vault://config-bundle",
		"oauth_client_secret":"vault://config-secret"
	}`)

	summary, err := mcpBackendSummary(definition, config, refs)
	if err != nil {
		t.Fatalf("mcpBackendSummary: %v", err)
	}
	if !summary.EndpointConfigured || !summary.BearerConfigured || !summary.OauthClientIdConfigured || !summary.OauthClientSecretConfigured {
		t.Fatalf("MCP summary flags = %#v, want all configured flags true", summary)
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal MCP summary: %v", err)
	}
	for _, forbidden := range []string{
		"private.example", "definition-secret", "config-secret", "vault://", "public-client-id", "config-client-id", "metadata",
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("MCP backend summary exposed %q: %s", forbidden, encoded)
		}
	}
}

func TestPluginCLIBackendSummaryOmitsResourceSecrets(t *testing.T) {
	definition := pluginpkg.Definition{Backend: pluginpkg.BackendCLI}
	config := pluginpkg.Config{Payload: json.RawMessage(`{"prompt":"static secret","binaries":[{"name":"tool","tool":"private/repo","version":"1.2.3","options":{"token":"secret"}}],"skills":[{"repo":"private/repo","name":"skill"}],"session_env":[{"env_var":"TOKEN","source":"literal","value":"secret","required":true}],"oauth_provider":"github"}`)}
	summary, err := pluginBackendSummary(definition, config)
	if err != nil {
		t.Fatalf("pluginBackendSummary: %v", err)
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal backend summary: %v", err)
	}
	for _, forbidden := range []string{"private/repo", "token", "secret", "prompt", "github"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("CLI backend summary exposed %q: %s", forbidden, encoded)
		}
	}
}

func TestPluginCLIBackendSummaryUsesDefinitionForDefaultConfig(t *testing.T) {
	definition := pluginpkg.Definition{
		Backend: pluginpkg.BackendCLI,
		Spec:    json.RawMessage(`{"binaries":[{"name":"tool","tool":"uv","version":"1.0"}],"skills":[{"name":"docs","repo":"owner/docs"}],"session_env":[{"env_var":"STELLA_TOKEN","source":"oauth.token","required":true}]}`),
	}
	summary, err := pluginBackendSummary(definition, pluginpkg.Config{Enabled: nil, Payload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	cli, err := summary.AsPluginCLIBackendSummary()
	if err != nil {
		t.Fatal(err)
	}
	if len(cli.Binaries) != 1 || cli.Binaries[0].Version != "1.0" || len(cli.Skills) != 1 || len(cli.SessionEnv) != 1 {
		t.Fatalf("default summary = %#v, want shipped resources", cli)
	}
}

func TestPluginCLIBackendSummaryHonorsScopeOverlayAndNegativeConfig(t *testing.T) {
	definition := pluginpkg.Definition{
		Backend: pluginpkg.BackendCLI,
		Spec:    json.RawMessage(`{"binaries":[{"name":"tool","tool":"uv","version":"1.0"},{"name":"other","tool":"bun","version":"1.0"}],"skills":[{"name":"docs","repo":"owner/docs"}]}`),
	}
	enabled := true
	summary, err := pluginBackendSummary(definition, pluginpkg.Config{Enabled: &enabled, Payload: json.RawMessage(`{"binaries":[{"name":"tool","version":"2.0"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	cli, err := summary.AsPluginCLIBackendSummary()
	if err != nil {
		t.Fatal(err)
	}
	if len(cli.Binaries) != 1 || cli.Binaries[0].Name != "tool" || cli.Binaries[0].Version != "2.0" {
		t.Fatalf("overlay summary = %#v, want target scope payload", cli)
	}

	disabled := false
	summary, err = pluginBackendSummary(definition, pluginpkg.Config{Enabled: &disabled})
	if err != nil {
		t.Fatal(err)
	}
	cli, err = summary.AsPluginCLIBackendSummary()
	if err != nil {
		t.Fatal(err)
	}
	if len(cli.Binaries) != 0 || len(cli.Skills) != 0 {
		t.Fatalf("negative summary = %#v, want no selected resources", cli)
	}
}

func TestPluginGoBackendSummaryOmitsBackendPayload(t *testing.T) {
	definition := pluginpkg.Definition{ID: "channel/telegram/bot", Backend: pluginpkg.BackendGo}
	config := pluginpkg.Config{Payload: json.RawMessage(`{"token":"channel-secret","endpoint":"private.example"}`)}
	summary, err := pluginBackendSummary(definition, config)
	if err != nil {
		t.Fatalf("pluginBackendSummary: %v", err)
	}
	goSummary, err := summary.AsPluginGoBackendSummary()
	if err != nil {
		t.Fatalf("decode Go backend summary: %v", err)
	}
	if !goSummary.Configured || goSummary.Kind == nil || *goSummary.Kind != "channel" {
		t.Fatalf("Go backend summary = %#v, want configured channel", goSummary)
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal Go backend summary: %v", err)
	}
	for _, forbidden := range []string{"channel-secret", "private.example", "token", "endpoint"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("Go backend summary exposed %q: %s", forbidden, encoded)
		}
	}
}

func TestRawContainsAnyKeyFindsNestedCredentialFields(t *testing.T) {
	data := []byte(`{"initial_config":{"config":{"credential_refs":{"token":"vault://secret"}}}}`)
	if !rawContainsAnyKey(data, "credentials", "credential_refs") {
		t.Fatal("nested credential field was accepted")
	}
	if rawContainsAnyKey([]byte(`{"config":{"endpoint":"https://example.test"}}`), "credentials", "credential_refs") {
		t.Fatal("safe config field was rejected")
	}
}

func TestWritePluginErrorMapsUnifiedCRUDErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{name: "scope", err: pluginpkg.ErrUnknownScope, want: http.StatusBadRequest},
		{name: "cas", err: pluginpkg.ErrConflict, want: http.StatusConflict},
		{name: "builtin", err: pluginpkg.ErrBuiltinConfig, want: http.StatusConflict},
		{name: "private definition", err: authz.ErrNotFound, want: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writePluginError(recorder, tc.err)
			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.want)
			}
		})
	}
}
