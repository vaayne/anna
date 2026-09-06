package host

import (
	"testing"

	"github.com/CherryHQ/stella/internal/plugin"
)

func TestGoConfigCannotReplaceDomainCredentials(t *testing.T) {
	def := plugin.Definition{ID: "channel/test", Namespace: "test", DisplayName: "Test", Backend: plugin.BackendGo, Source: plugin.SourceBuiltin, ImplementationKey: "channel/test", Spec: []byte(`{}`), DefaultEnabled: true, Revision: 1}
	cfg := plugin.Config{ID: "20000000-0000-0000-0000-000000000096", PluginID: def.ID, Namespace: def.Namespace, Scope: plugin.ScopeSystem, Payload: []byte(`{}`), CredentialRefs: []byte(`{}`), Revision: 1}
	if err := ValidatePayload(t.Context(), def, cfg, nil); err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{`{"token":"account-token"}`, `{"accounts":[]}`, `{"essential":true}`} {
		candidate := cfg
		candidate.Payload = []byte(payload)
		if err := ValidatePayload(t.Context(), def, candidate, nil); err == nil {
			t.Fatalf("accepted domain fields: %s", payload)
		}
	}
	cfg.CredentialRefs = []byte(`{"token":{"name":"MCP_TOKEN_TEST"}}`)
	if err := ValidatePayload(t.Context(), def, cfg, nil); err == nil {
		t.Fatal("accepted channel credential reference in platform cap")
	}
}
