package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/CherryHQ/stella/internal/mcp"
)

func TestAgentMCPServerResponseOmitsPrivateRegistrationFields(t *testing.T) {
	response := agentMCPServerResponse(mcp.Registration{
		ID: "config-1", PluginID: "mcp/github", Namespace: "github",
		Scope: "system", URL: "https://secret.example.test/mcp",
		CredentialRef: "vault://credential", AuthType: "bearer",
		Status: mcp.StatusOK, CredentialMode: "shared", Enabled: true,
		Tools: []mcp.CatalogTool{{Name: "search"}},
	}, false)
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, private := range []string{"secret.example.test", `"url"`, `"payload"`, `"metadata"`, `"credential_ref"`, `"credential_refs"`} {
		if strings.Contains(body, private) {
			t.Fatalf("safe effective response contains %q: %s", private, body)
		}
	}
	if response.Readable || response.PluginId != "mcp/github" || response.ConfigId != "config-1" {
		t.Fatalf("unexpected identity/readability projection: %+v", response)
	}
}
