package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/CherryHQ/stella/pkg/db/pgnull"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
)

func TestToolProviderConnectDiagnosticRedactsEndpointSecrets(t *testing.T) {
	const raw = "https://user:canary-userinfo@mcp.example/tools?token=canary-query#canary-fragment"
	db := newFakeDB()
	db.forCtx = []sqlc.McpServer{{
		ID: "broken", Scope: ScopeUser, UserID: pgnull.Text("u1"), Name: "broken", Url: raw,
		Transport: TransportStreamableHTTP, AuthType: AuthTypeNone, Enabled: true,
	}}
	svc := NewService(db, newFakeVault())
	provider := NewToolProvider(svc)
	svc.connect = func(_ context.Context, reg Registration, _ CredentialOwner) (RemoteClient, error) {
		return nil, connectionError(reg, context.DeadlineExceeded)
	}
	var logs bytes.Buffer
	provider.log = slog.New(slog.NewTextHandler(&logs, nil))

	reg := registrationFromRow(db.forCtx[0])
	reg.Namespace = "broken"
	if tools := provider.toolsForRegistrations(context.Background(), []Registration{reg}, true, "u1"); len(tools) != 0 {
		t.Fatalf("provider returned %d tools, want none", len(tools))
	}
	got := logs.String()
	for _, secret := range []string{"canary-userinfo", "canary-query", "canary-fragment"} {
		if strings.Contains(got, secret) {
			t.Fatalf("MCP diagnostic leaked %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "https://mcp.example/tools") {
		t.Fatalf("MCP diagnostic lost useful endpoint: %s", got)
	}
}

func TestConnectionErrorPreservesCause(t *testing.T) {
	cause := context.DeadlineExceeded
	err := connectionError(Registration{URL: "https://mcp.example/tools?token=canary-query"}, cause)
	if !errors.Is(err, cause) {
		t.Fatalf("errors.Is(%v, %v) = false, want true", err, cause)
	}
	if strings.Contains(err.Error(), "canary-query") {
		t.Fatalf("connection error leaked endpoint secret: %s", err)
	}
}

func TestConnectionErrorKeepsSafeValidationDetail(t *testing.T) {
	cause := errors.New("mcp: endpoint url must use http or https")
	err := connectionError(Registration{URL: "ftp://mcp.example/tools"}, cause)
	if !errors.Is(err, cause) {
		t.Fatalf("errors.Is(%v, cause) = false, want true", err)
	}
	if !strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("connection error dropped safe validation detail: %s", err)
	}

	const secretTransport = "canary-secret-transport"
	transportErr := connectionError(
		Registration{URL: "https://mcp.example/tools"},
		fmt.Errorf("mcp: unsupported transport %q", secretTransport),
	)
	if strings.Contains(transportErr.Error(), secretTransport) {
		t.Fatalf("connection error leaked transport input: %s", transportErr)
	}
	if !strings.Contains(transportErr.Error(), "only streamable_http and sse are allowed") {
		t.Fatalf("connection error dropped safe transport guidance: %s", transportErr)
	}
}
