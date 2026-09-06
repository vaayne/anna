package manifest

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/CherryHQ/stella/internal/plugin"
)

func testCLIDefinition(t *testing.T) plugin.Definition {
	t.Helper()
	spec, err := json.Marshal(cliPayload{
		Description: "release",
		Category:    "system",
		Prompt:      "use the cli",
		Binaries: []ManifestBinary{{
			Name: "demo", Tool: "github:owner/demo", Version: "1.0.0",
			Options: map[string]any{"asset_pattern": "demo_*", "future_option": "published"},
		}},
		Skills:        []ManifestSkill{{Name: "demo"}},
		SessionEnvs:   []ManifestSessionEnv{{EnvVar: "DEMO_TOKEN", Source: "oauth.access_token", Required: true}},
		OAuthProvider: "demo",
	})
	if err != nil {
		t.Fatal(err)
	}
	return plugin.Definition{
		ID: "tool/demo", Namespace: "demo", DisplayName: "Demo",
		Backend: plugin.BackendCLI, Source: plugin.SourceBuiltin,
		ImplementationKey: "tool/demo", Spec: spec, DefaultEnabled: true, Revision: 1,
	}
}

func testUserPayload(t *testing.T, version string) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(cliPayload{
		Description: "release",
		Category:    "system",
		Prompt:      "use the cli",
		Binaries: []ManifestBinary{{
			Name: "demo", Tool: "github:owner/demo", Version: version,
			Options: map[string]any{"asset_pattern": "demo_*", "future_option": "published"},
		}},
		Skills:        []ManifestSkill{{Name: "demo"}},
		SessionEnvs:   []ManifestSessionEnv{{EnvVar: "DEMO_TOKEN", Source: "oauth.refresh_token", Required: true}},
		OAuthProvider: "demo",
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestValidatePayloadAcceptsSystemAndUserOwnership(t *testing.T) {
	definition := testCLIDefinition(t)
	system := plugin.Config{ID: "system", PluginID: definition.ID, Namespace: definition.Namespace, Scope: plugin.ScopeSystem, Revision: 1, Enabled: boolPtr(true), Payload: definition.Spec}
	if err := ValidatePayload(t.Context(), definition, system, nil); err != nil {
		t.Fatalf("system payload: %v", err)
	}

	user := plugin.Config{ID: "user", PluginID: definition.ID, Namespace: definition.Namespace, Scope: plugin.ScopeUser, UserID: "user-1", Revision: 1, Enabled: boolPtr(true), Payload: testUserPayload(t, "2.0.0"), CredentialRefs: json.RawMessage(`{"session_env":{"name":"DEMO_OAUTH","scope":"user","user_id":"user-1"}}`)}
	if err := ValidatePayload(t.Context(), definition, user, nil); err != nil {
		t.Fatalf("user payload: %v", err)
	}
	var userPayload cliPayload
	if err := json.Unmarshal(testUserPayload(t, "2.0.0"), &userPayload); err != nil {
		t.Fatal(err)
	}
	userPayload.Binaries[0].Options["extras"] = "spelling"
	encoded, err := json.Marshal(userPayload)
	if err != nil {
		t.Fatal(err)
	}
	user.Payload = encoded
	if err := ValidatePayload(t.Context(), definition, user, nil); err != nil {
		t.Fatalf("user safe option: %v", err)
	}
}

func TestValidatePayloadRejectsUserResourceIdentityChanges(t *testing.T) {
	definition := testCLIDefinition(t)
	cases := []struct {
		name   string
		mutate func(*cliPayload)
		resets []string
	}{
		{"binary name", func(p *cliPayload) { p.Binaries[0].Name = "other" }, nil},
		{"binary tool", func(p *cliPayload) { p.Binaries[0].Tool = "github:other/demo" }, nil},
		{"binary options", func(p *cliPayload) { p.Binaries[0].Options = map[string]any{"bin_path": "/tmp"} }, nil},
		{"published option changed", func(p *cliPayload) { p.Binaries[0].Options["future_option"] = "changed" }, nil},
		{"published option removed", func(p *cliPayload) { delete(p.Binaries[0].Options, "future_option") }, nil},
		{"extras type", func(p *cliPayload) { p.Binaries[0].Options["extras"] = true }, nil},
		{"unknown binary option", func(p *cliPayload) { p.Binaries[0].Options["new_hook"] = true }, nil},
		{"skill", func(p *cliPayload) { p.Skills[0].Name = "other" }, nil},
		{"prompt", func(p *cliPayload) { p.Prompt = "run anything" }, nil},
		{"session env identity", func(p *cliPayload) { p.SessionEnvs[0].EnvVar = "OTHER" }, nil},
		{"session env required", func(p *cliPayload) { p.SessionEnvs[0].Required = false }, nil},
		{"provider", func(p *cliPayload) { p.OAuthProvider = "other" }, nil},
		{"reset prompt", func(*cliPayload) {}, []string{"prompt"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var payload cliPayload
			if err := json.Unmarshal(testUserPayload(t, "2.0.0"), &payload); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&payload)
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			config := plugin.Config{ID: "user", PluginID: definition.ID, Namespace: definition.Namespace, Scope: plugin.ScopeUser, UserID: "user-1", Revision: 1, Enabled: boolPtr(true), Payload: raw}
			if err := ValidatePayload(t.Context(), definition, config, tc.resets); !errors.Is(err, plugin.ErrInvalidConfig) {
				t.Fatalf("error = %v, want invalid config", err)
			}
		})
	}
}

func TestValidatePayloadRejectsSecretsUnknownFieldsAndBadRefs(t *testing.T) {
	definition := testCLIDefinition(t)
	cases := []struct {
		name      string
		payload   string
		refs      string
		wantError string
	}{
		{"unknown payload field", `{"unexpected":true}`, `{}`, "unknown field"},
		{"secret session env value", string(testUserPayload(t, "2.0.0")), `{"session_env":{"name":"x","scope":"user","user_id":"user-1"}}`, "value"},
		{"wrong ref owner", string(testUserPayload(t, "2.0.0")), `{"session_env":{"name":"x","scope":"user","user_id":"other"}}`, "owner"},
		{"plaintext ref", string(testUserPayload(t, "2.0.0")), `{"session_env":{"name":"x","scope":"user","user_id":"user-1","token":"secret"}}`, "unknown field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := tc.payload
			if tc.name == "secret session env value" {
				var decoded cliPayload
				if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
					t.Fatal(err)
				}
				decoded.SessionEnvs[0].Value = "secret"
				encoded, err := json.Marshal(decoded)
				if err != nil {
					t.Fatal(err)
				}
				payload = string(encoded)
			}
			config := plugin.Config{ID: "user", PluginID: definition.ID, Namespace: definition.Namespace, Scope: plugin.ScopeUser, UserID: "user-1", Revision: 1, Enabled: boolPtr(false), Payload: json.RawMessage(payload), CredentialRefs: json.RawMessage(tc.refs)}
			err := ValidatePayload(t.Context(), definition, config, nil)
			if !errors.Is(err, plugin.ErrInvalidConfig) || !contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want invalid config containing %q", err, tc.wantError)
			}
		})
	}
}

func TestValidatePayloadChecksDisabledPayloadAndResetFields(t *testing.T) {
	definition := testCLIDefinition(t)
	disabled := plugin.Config{ID: "user", PluginID: definition.ID, Namespace: definition.Namespace, Scope: plugin.ScopeUser, UserID: "user-1", Revision: 1, Enabled: boolPtr(false), Payload: json.RawMessage(`{"unexpected":true}`)}
	if err := ValidatePayload(t.Context(), definition, disabled, nil); !errors.Is(err, plugin.ErrInvalidConfig) {
		t.Fatalf("disabled malformed payload = %v, want invalid config", err)
	}
	if err := ValidatePayload(t.Context(), definition, plugin.Config{ID: "user", PluginID: definition.ID, Namespace: definition.Namespace, Scope: plugin.ScopeUser, UserID: "user-1", Revision: 1, Enabled: boolPtr(false), Payload: testUserPayload(t, "2.0.0")}, []string{"description"}); !errors.Is(err, plugin.ErrInvalidConfig) {
		t.Fatalf("disabled unauthorized reset = %v, want invalid config", err)
	}
	var malformed cliPayload
	if err := json.Unmarshal(definition.Spec, &malformed); err != nil {
		t.Fatal(err)
	}
	malformed.Binaries[0].Name = ""
	encoded, err := json.Marshal(malformed)
	if err != nil {
		t.Fatal(err)
	}
	definition.Spec = encoded
	if err := ValidatePayload(t.Context(), definition, plugin.Config{ID: "negative", PluginID: definition.ID, Namespace: definition.Namespace, Scope: plugin.ScopeUser, UserID: "user-1", Revision: 1, Enabled: boolPtr(false)}, nil); !errors.Is(err, plugin.ErrInvalidConfig) {
		t.Fatalf("nil payload with malformed definition = %v, want invalid config", err)
	}
}

func boolPtr(value bool) *bool { return &value }

func contains(value, want string) bool {
	return strings.Contains(value, want)
}
