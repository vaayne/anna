package mcp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/plugin"
)

func TestRegistrationFromPluginConfigPreservesIdentityRefsAndRemoteNames(t *testing.T) {
	const id = "0198f9a4-1b2c-7def-8123-456789abcdef"
	def := plugin.Definition{
		ID: "custom/" + id, Namespace: "github", DisplayName: "GitHub MCP",
		Backend: plugin.BackendMCP, Source: plugin.SourceCustom, ImplementationKey: "mcp",
		Spec: []byte(`{}`), Revision: 1,
	}
	cfg := plugin.Config{
		ID: id, PluginID: def.ID, Namespace: def.Namespace, Scope: plugin.ScopeSystem,
		Enabled: boolPtr(true), Payload: []byte(`{"url":"https://mcp.example.test/path","transport":"streamable_http","auth_type":"oauth","credential_mode":"per_user","metadata":{"oauth":{"client_id":"client-123"}}}`),
		CredentialRefs: []byte(`{"oauth_bundle":{"name":"MCP_OAUTH_0198F9A4_1B2C_7DEF_8123_456789ABCDEF","mode":"per_user","owner":"per_user"},"oauth_client_secret":{"name":"MCP_OAUTH_CLIENT_0198F9A4_1B2C_7DEF_8123_456789ABCDEF","scope":"system","user_id":"","agent_id":""}}`),
		Revision:       1,
	}
	effective := plugin.Effective{PluginID: def.ID, Namespace: def.Namespace, ConfigID: cfg.ID, SourceScope: plugin.ScopeSystem, IsEffectivelyEnabled: true, Payload: cfg.Payload}
	observed := PluginMCPObservation{
		Status: StatusError, StatusError: "remote https://mcp.example.test/path?token=secret failed", ConfigRevision: 1,
		CredentialUserID: "user-1",
		ProbedAt:         time.Date(2026, 9, 6, 1, 2, 3, 0, time.FixedZone("CST", 8*60*60)),
		Tools:            []CatalogTool{{Name: "create-issue", Description: "Create an issue", InputSchema: map[string]any{"type": "object"}}},
	}
	reg, err := RegistrationFromPluginConfig(def, cfg, effective, observed, testUserAuthority(t, "user-1"))
	if err != nil {
		t.Fatal(err)
	}
	if reg.ID != id || reg.Name != def.DisplayName || reg.Scope != ScopeSystem || reg.CredentialMode != CredentialModePerUser {
		t.Fatalf("registration identity = %#v", reg)
	}
	if reg.CredentialRef != "" || reg.OAuthClientID != "client-123" || reg.OAuthClientSecretRef != "MCP_OAUTH_CLIENT_0198F9A4_1B2C_7DEF_8123_456789ABCDEF" || reg.Enabled != true || reg.Namespace != def.Namespace || reg.ConfigRevision != 1 {
		t.Fatalf("credential projection = %#v", reg)
	}
	if reg.StatusError != "MCP probe failed" || strings.Contains(reg.StatusError, "secret") {
		t.Fatalf("observation was not redacted: %q", reg.StatusError)
	}
	if len(reg.Tools) != 1 || reg.Tools[0].Name != "create-issue" {
		t.Fatalf("remote tool name was rewritten: %#v", reg.Tools)
	}
	if got := reg.ProbedAt.Location().String(); got != "UTC" || reg.ProbedAt.Hour() != 17 {
		t.Fatalf("probe timestamp = %s %v", reg.ProbedAt, reg.ProbedAt.Location())
	}
}

func TestMCPConverterMatchesNormalizedLegacyOAuthShape(t *testing.T) {
	const id = "0198f9a4-1b2c-7def-8123-456789abcdef"
	plan, err := plugin.NormalizeLegacySnapshot(plugin.LegacySnapshot{MCP: []plugin.LegacyMCPRegistration{{
		ID: id, Scope: string(plugin.ScopeUser), UserID: "user-1", Name: "GitHub MCP", URL: "https://mcp.example.test",
		Transport: TransportStreamableHTTP, AuthType: AuthTypeOAuth, CredentialMode: CredentialModeShared, Enabled: true,
		Metadata: map[string]any{
			"oauth":    map[string]any{"client_id": "client-123", "client_secret_ref": "MCP_OAUTH_CLIENT_0198F9A4_1B2C_7DEF_8123_456789ABCDEF"},
			"registry": map[string]any{"source": "official", "id": "github", "version": "1"},
		},
		OAuthClientSecretExists: true,
	}}}, plugin.NewCatalog(), nil)
	if err != nil {
		t.Fatal(err)
	}
	def, cfg := plan.Definitions[0], plan.Configs[0]
	def.Spec = []byte(`{"description":"safe MCP description"}`)
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(cfg.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload["description"] = json.RawMessage(`"safe MCP description"`)
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Payload = payloadBytes
	effective := plugin.Effective{PluginID: def.ID, Namespace: def.Namespace, ConfigID: cfg.ID, SourceScope: cfg.Scope, IsEffectivelyEnabled: true, Payload: payloadBytes}
	if err := ValidateMCPPayload(t.Context(), EndpointPolicy{}, def, cfg, nil); err != nil {
		t.Fatalf("normalized MCP payload rejected: %v", err)
	}
	reg, err := RegistrationFromPluginConfig(def, cfg, effective, PluginMCPObservation{ConfigRevision: cfg.Revision, Tools: []CatalogTool{{Name: "original-remote"}}}, testUserAuthority(t, "user-1"))
	if err != nil {
		t.Fatal(err)
	}
	if reg.ID != id || reg.PluginID != def.ID || reg.Namespace != def.Namespace || reg.Description != "safe MCP description" || reg.OAuthClientID != "client-123" || reg.OAuthClientSecretRef != "MCP_OAUTH_CLIENT_0198F9A4_1B2C_7DEF_8123_456789ABCDEF" || reg.CredentialMode != CredentialModeShared || reg.Tools[0].Name != "original-remote" {
		t.Fatalf("normalized legacy conversion = %#v", reg)
	}
}

func TestRegistrationFromPluginConfigRejectsAuthPayloadResidue(t *testing.T) {
	def, cfg, effective := testPluginMCPInputs()
	for name, payload := range map[string]string{
		"display name":      `{"url":"https://mcp.example.test","transport":"sse","auth_type":"none","name":"shadow"}`,
		"tools observation": `{"url":"https://mcp.example.test","transport":"sse","auth_type":"none","tools":[]}`,
		"endpoint secret":   `{"url":"https://mcp.example.test","transport":"sse","auth_type":"none","endpoint_secret":"secret"}`,
	} {
		t.Run(name, func(t *testing.T) {
			effective.Payload = json.RawMessage(payload)
			if _, err := RegistrationFromPluginConfig(def, cfg, effective, PluginMCPObservation{}, testUserAuthority(t, "user-1")); err == nil {
				t.Fatal("accepted unsupported payload field")
			}
		})
	}
}

func TestRegistrationFromPluginConfigRejectsCredentialOwnerMismatch(t *testing.T) {
	def, cfg, effective := testPluginMCPInputs()
	cfg.Payload = []byte(`{"url":"https://mcp.example.test","transport":"sse","auth_type":"bearer"}`)
	effective.Payload = cfg.Payload
	cfg.CredentialRefs = []byte(`{"bearer":{"name":"MCP_TOKEN_legacy","scope":"user","user_id":"other-user"}}`)
	if _, err := RegistrationFromPluginConfig(def, cfg, effective, PluginMCPObservation{}, testUserAuthority(t, "user-1")); err == nil {
		t.Fatal("accepted bearer locator for another owner")
	}
}

func TestRegistrationFromPluginConfigRejectsUnexpectedBearerRef(t *testing.T) {
	def, cfg, effective := testPluginMCPInputs()
	cfg.Payload = []byte(`{"url":"https://mcp.example.test","transport":"sse","auth_type":"bearer"}`)
	cfg.CredentialRefs = []byte(`{"bearer":{"name":"MCP_TOKEN_OTHER","scope":"user","user_id":"user-1","agent_id":""}}`)
	effective.Payload = cfg.Payload
	if _, err := RegistrationFromPluginConfig(def, cfg, effective, PluginMCPObservation{}, testUserAuthority(t, "user-1")); err == nil {
		t.Fatal("unexpected bearer locator must be rejected")
	}
}

func TestRegistrationFromPluginConfigRejectsSecretWithoutClientID(t *testing.T) {
	def, cfg, effective := testPluginMCPInputs()
	cfg.Payload = []byte(`{"url":"https://mcp.example.test","transport":"sse","auth_type":"oauth"}`)
	cfg.CredentialRefs = []byte(`{"oauth_bundle":{"name":"MCP_OAUTH_0198F9A4_1B2C_7DEF_8123_456789ABCDEF","mode":"shared","scope":"user","user_id":"user-1","agent_id":""},"oauth_client_secret":{"name":"MCP_OAUTH_CLIENT_0198F9A4_1B2C_7DEF_8123_456789ABCDEF","scope":"user","user_id":"user-1","agent_id":""}}`)
	effective.Payload = cfg.Payload
	if _, err := RegistrationFromPluginConfig(def, cfg, effective, PluginMCPObservation{}, testUserAuthority(t, "user-1")); err == nil {
		t.Fatal("accepted OAuth client secret without client id")
	}
}

func TestDecodeMCPPluginMetadataNormalizesTokenEndpointAuthMethod(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "omitted defaults basic", raw: `{"oauth":{"client_id":"client-123"}}`, want: ""},
		{name: "explicit empty defaults basic", raw: `{"oauth":{"client_id":"client-123","token_endpoint_auth_method":""}}`, want: oauthTokenEndpointAuthMethodBasic},
		{name: "basic", raw: `{"oauth":{"token_endpoint_auth_method":"client_secret_basic"}}`, want: oauthTokenEndpointAuthMethodBasic},
		{name: "post", raw: `{"oauth":{"token_endpoint_auth_method":"client_secret_post"}}`, want: oauthTokenEndpointAuthMethodPost},
		{name: "none", raw: `{"oauth":{"token_endpoint_auth_method":"none"}}`, want: oauthTokenEndpointAuthMethodNone},
	} {
		t.Run(tt.name, func(t *testing.T) {
			metadata, err := decodeMCPPluginMetadata(json.RawMessage(tt.raw))
			if err != nil {
				t.Fatal(err)
			}
			oauth, ok := metadata["oauth"].(map[string]any)
			if !ok {
				t.Fatal("oauth metadata was not decoded as an object")
			}
			if tt.want == "" {
				if _, ok := oauth["token_endpoint_auth_method"]; ok {
					t.Fatalf("omitted auth method was unexpectedly materialized: %#v", oauth)
				}
				return
			}
			if got := oauth["token_endpoint_auth_method"]; got != tt.want {
				t.Fatalf("normalized auth method = %#v, want %q", got, tt.want)
			}
		})
	}

	for _, raw := range []string{
		`{"oauth":{"token_endpoint_auth_method":"private_key_jwt"}}`,
		`{"oauth":{"token_endpoint_auth_method":null}}`,
		`{"oauth":{"token_endpoint_auth_method":true}}`,
	} {
		if _, err := decodeMCPPluginMetadata(json.RawMessage(raw)); err == nil {
			t.Fatalf("unsupported OAuth token endpoint auth metadata accepted: %s", raw)
		}
	}
}

func TestValidateMCPPayloadAppliesEndpointSafetyWhenDisabled(t *testing.T) {
	def, cfg, _ := testPluginMCPInputs()
	cfg.Enabled = boolPtr(false)
	cfg.Payload = []byte(`{"url":"https://user:secret@example.test/path?token=secret","transport":"sse","auth_type":"none"}`)
	if err := ValidateMCPPayload(t.Context(), EndpointPolicy{}, def, cfg, nil); err == nil {
		t.Fatal("disabled config accepted unsafe endpoint")
	}

	cfg.Payload = nil
	cfg.CredentialRefs = []byte(`{"bearer":{"name":"secret"}}`)
	if err := ValidateMCPPayload(t.Context(), EndpointPolicy{}, def, cfg, nil); !errors.Is(err, plugin.ErrInvalidConfig) {
		t.Fatalf("negative config refs error = %v", err)
	}
}

func TestRegistrationFromPluginConfigTreatsStaleObservationAsUnknown(t *testing.T) {
	def, cfg, effective := testPluginMCPInputs()
	reg, err := RegistrationFromPluginConfig(def, cfg, effective, PluginMCPObservation{Status: StatusOK, ConfigRevision: cfg.Revision + 1, Tools: []CatalogTool{{Name: "stale"}}}, testUserAuthority(t, "user-1"))
	if err != nil {
		t.Fatal(err)
	}
	if reg.Status != StatusUnknown || reg.StatusError != "" || !reg.ProbedAt.IsZero() || len(reg.Tools) != 0 {
		t.Fatalf("stale observation retained: %#v", reg)
	}
}

func TestRegistrationFromPluginConfigRejectsInvalidAuthority(t *testing.T) {
	def, cfg, effective := testPluginMCPInputs()
	_, err := RegistrationFromPluginConfig(def, cfg, effective, PluginMCPObservation{}, authz.Authority{})
	if !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("invalid authority error = %v", err)
	}
}

func TestPerUserObservationIsolatedByTrustedOwner(t *testing.T) {
	def, cfg, effective := testPerUserPluginMCPInputs()
	authorityA := testUserAuthority(t, "user-a")
	authorityB := testUserAuthority(t, "user-b")
	observationA := PluginMCPObservation{
		Status: StatusOK, ConfigRevision: cfg.Revision, CredentialUserID: "user-a",
		Tools: []CatalogTool{{Name: "alpha", Description: "A only", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}}},
	}
	observationB := PluginMCPObservation{
		Status: StatusNeedsAuth, StatusError: "user B credential rejected", ConfigRevision: cfg.Revision, CredentialUserID: "user-b",
		Tools: []CatalogTool{{Name: "beta", Description: "B only", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"b": map[string]any{"type": "integer"}}}}},
	}

	regA, err := RegistrationFromPluginConfig(def, cfg, effective, observationA, authorityA)
	if err != nil {
		t.Fatal(err)
	}
	if regA.Status != StatusOK || len(regA.Tools) != 1 || regA.Tools[0].Name != "alpha" || regA.Tools[0].Description != "A only" {
		t.Fatalf("user A observation = %#v", regA)
	}
	regB, err := RegistrationFromPluginConfig(def, cfg, effective, observationB, authorityB)
	if err != nil {
		t.Fatal(err)
	}
	if regB.Status != StatusNeedsAuth || len(regB.Tools) != 1 || regB.Tools[0].Name != "beta" || regB.StatusError != credentialRejectedHint {
		t.Fatalf("user B observation = %#v", regB)
	}

	foreign, err := RegistrationFromPluginConfig(def, cfg, effective, observationB, authorityA)
	if err != nil {
		t.Fatal(err)
	}
	if foreign.Status != StatusUnknown || foreign.StatusError != "" || !foreign.ProbedAt.IsZero() || len(foreign.Tools) != 0 {
		t.Fatalf("foreign observation was retained: %#v", foreign)
	}
	malformedForeign := observationB
	malformedForeign.Status = "invalid-status"
	malformedForeign.Tools = []CatalogTool{{Name: ""}}
	foreign, err = RegistrationFromPluginConfig(def, cfg, effective, malformedForeign, authorityA)
	if err != nil {
		t.Fatal(err)
	}
	if foreign.Status != StatusUnknown || len(foreign.Tools) != 0 {
		t.Fatalf("malformed foreign observation was inspected: %#v", foreign)
	}
}

func testPluginMCPInputs() (plugin.Definition, plugin.Config, plugin.Effective) {
	const id = "0198f9a4-1b2c-7def-8123-456789abcdef"
	def := plugin.Definition{ID: "custom/" + id, Namespace: "mcp_test", DisplayName: "MCP test", Backend: plugin.BackendMCP, Source: plugin.SourceCustom, ImplementationKey: "mcp", Spec: []byte(`{}`), Revision: 1}
	cfg := plugin.Config{ID: id, PluginID: def.ID, Namespace: def.Namespace, Scope: plugin.ScopeUser, UserID: "user-1", Enabled: boolPtr(true), Payload: []byte(`{"url":"https://mcp.example.test","transport":"sse","auth_type":"none"}`), CredentialRefs: []byte(`{}`), Revision: 1}
	effective := plugin.Effective{PluginID: def.ID, Namespace: def.Namespace, ConfigID: cfg.ID, SourceScope: plugin.ScopeUser, IsEffectivelyEnabled: true, Payload: cfg.Payload}
	return def, cfg, effective
}

func testPerUserPluginMCPInputs() (plugin.Definition, plugin.Config, plugin.Effective) {
	const id = "0198f9a4-1b2c-7def-8123-456789abcdef"
	def := plugin.Definition{ID: "custom/" + id, Namespace: "mcp_test", DisplayName: "MCP per-user test", Backend: plugin.BackendMCP, Source: plugin.SourceCustom, ImplementationKey: "mcp", Spec: []byte(`{}`), Revision: 1}
	cfg := plugin.Config{ID: id, PluginID: def.ID, Namespace: def.Namespace, Scope: plugin.ScopeSystem, Enabled: boolPtr(true), Payload: []byte(`{"url":"https://mcp.example.test","transport":"streamable_http","auth_type":"oauth","credential_mode":"per_user","metadata":{"oauth":{"client_id":"client-123"}}}`), Revision: 1}
	refs, _ := json.Marshal(map[string]any{"oauth_bundle": map[string]any{"name": oauthBundleName(id), "mode": CredentialModePerUser, "owner": "per_user"}})
	cfg.CredentialRefs = refs
	effective := plugin.Effective{PluginID: def.ID, Namespace: def.Namespace, ConfigID: cfg.ID, SourceScope: cfg.Scope, IsEffectivelyEnabled: true, Payload: cfg.Payload}
	return def, cfg, effective
}

func testUserAuthority(t *testing.T, userID string) authz.Authority {
	t.Helper()
	authority, err := authz.NewUserAuthority(authz.UserID(userID), false)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}
