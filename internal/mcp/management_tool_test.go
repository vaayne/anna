package mcp

import (
	"context"
	"strings"
	"testing"
)

func TestManagementProjectionRedactsLegacyEndpoint(t *testing.T) {
	view := managementProjection(Registration{
		ID: "legacy", Scope: ScopeUser, Name: "legacy",
		URL:       "https://user:token@example.test/mcp?access_token=leaked#fragment",
		Transport: TransportStreamableHTTP, AuthType: AuthTypeBearer,
	})
	if view.URL != "https://example.test" || !view.EndpointRedacted {
		t.Fatalf("legacy endpoint projection = %#v, want redacted origin", view)
	}
}

func TestManagementProjectionRedactsEndpointPathWithoutDroppingOrigin(t *testing.T) {
	view := managementProjection(Registration{
		ID: "path", Scope: ScopeUser, Name: "path",
		URL: "https://example.test:8443/mcp/token-like-path",
	})
	if view.URL != "https://example.test:8443" || !view.EndpointRedacted {
		t.Fatalf("path endpoint projection = %#v, want origin with redaction", view)
	}
	if view.URL == "" || view.URL == "https://example.test:8443/mcp/token-like-path" {
		t.Fatalf("path leaked through endpoint projection: %#v", view)
	}
}

func TestManagementProjectionKeepsPlainEndpointOrigin(t *testing.T) {
	view := managementProjection(Registration{URL: "https://example.test:8443"})
	if view.URL != "https://example.test:8443" || view.EndpointRedacted {
		t.Fatalf("plain endpoint projection = %#v, want unredacted origin", view)
	}
}

func TestMCPManagementInputsRejectCredentialFields(t *testing.T) {
	for _, tc := range []struct {
		action string
		args   map[string]any
	}{
		{action: "create", args: map[string]any{
			"name": "server", "scope": "user", "url": "https://example.test",
			"credential_mode": "per_user",
		}},
		{action: "create", args: map[string]any{
			"name": "server", "scope": "user", "url": "https://example.test",
			"source": "legacy",
		}},
		{action: "update", args: map[string]any{
			"id": "server", "expected_version": "v1", "oauth_client_secret": "secret",
		}},
	} {
		t.Run(tc.action, func(t *testing.T) {
			_, err := SettingsMcpDispatch(context.Background(), nil, tc.action, tc.args)
			if err == nil || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("SettingsMcpDispatch(%s) error = %v, want unknown field", tc.action, err)
			}
		})
	}
}
