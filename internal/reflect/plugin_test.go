package reflect

import (
	"context"
	"errors"
	"testing"

	"github.com/CherryHQ/stella/internal/authz"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	"github.com/CherryHQ/stella/internal/memory"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

func TestBuiltinPluginIsRegistered(t *testing.T) {
	if plugin, ok := pkgplugins.Get(PluginID); !ok || plugin == nil {
		t.Fatalf("plugin %q is not registered in the process catalog", PluginID)
	}
}

func TestAuthorizeTargetUsesTrustedOwnerAndReflectPlugin(t *testing.T) {
	authority, err := agentaccess.WorkerAgentAuthority("user-1", "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	called := false
	svc := &Service{capabilityGate: func(_ context.Context, gotAuthority authz.Authority, agentID string, pluginIDs ...string) error {
		called = true
		if gotAuthority != authority {
			t.Fatalf("authority = %#v, want %#v", gotAuthority, authority)
		}
		if agentID != "agent-1" {
			t.Fatalf("agentID = %q, want agent-1", agentID)
		}
		if len(pluginIDs) != 1 || pluginIDs[0] != PluginID {
			t.Fatalf("plugin IDs = %v, want [%s]", pluginIDs, PluginID)
		}
		return nil
	}}

	if err := svc.authorizeTarget(context.Background(), reviewTarget{session: memory.Session{ID: "session-1", UserID: "user-1", AgentID: "agent-1"}}); err != nil {
		t.Fatalf("authorizeTarget: %v", err)
	}
	if !called {
		t.Fatal("capability gate was not called")
	}
}

func TestAuthorizeTargetFailsClosedWithoutTrustedOwnerOrGate(t *testing.T) {
	tests := []struct {
		name   string
		svc    *Service
		target reviewTarget
	}{
		{name: "missing gate", svc: &Service{}, target: reviewTarget{session: memory.Session{ID: "session-1", UserID: "user-1", AgentID: "agent-1"}}},
		{name: "missing owner", svc: &Service{capabilityGate: func(context.Context, authz.Authority, string, ...string) error { return nil }}, target: reviewTarget{session: memory.Session{ID: "session-1", AgentID: "agent-1"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.svc.authorizeTarget(context.Background(), tt.target); err == nil {
				t.Fatal("authorizeTarget accepted an invalid public target")
			}
		})
	}

	denied := errors.New("disabled")
	svc := &Service{capabilityGate: func(context.Context, authz.Authority, string, ...string) error { return denied }}
	err := svc.authorizeTarget(context.Background(), reviewTarget{session: memory.Session{ID: "session-1", UserID: "user-1", AgentID: "agent-1"}})
	if !errors.Is(err, denied) {
		t.Fatalf("authorizeTarget error = %v, want wrapped gate error", err)
	}
}
