package channel

import (
	"context"
	"errors"
	"testing"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/plugin"
)

func TestChannelPluginGateDoesNotResolveGuestThroughOwnerSnapshot(t *testing.T) {
	called := false
	resolverErr := errors.New("owner snapshot must not be used")
	coord := &Coordinator{
		listenerCap: func(context.Context, string, string) (bool, error) { return false, nil },
		snapshotResolver: func(context.Context, authz.Authority, string) (plugin.Snapshot, error) {
			called = true
			return plugin.Snapshot{}, resolverErr
		},
	}

	allowed, err := coord.channelPluginAllowed(t.Context(), &ResolvedChat{
		GuestID: "guest-1",
		ChatCtx: ChatContext{Platform: "feishu"},
	})
	if err != nil || allowed {
		t.Fatalf("guest gate = %v, %v; want listener cap denial without owner snapshot", allowed, err)
	}
	if called {
		t.Fatal("guest gate resolved a snapshot using an owner path")
	}
}

func TestChannelPluginGatePropagatesSnapshotFailureBeforeDispatch(t *testing.T) {
	resolverErr := errors.New("snapshot unavailable")
	coord := &Coordinator{
		snapshotResolver: func(context.Context, authz.Authority, string) (plugin.Snapshot, error) {
			return plugin.Snapshot{}, resolverErr
		},
	}

	allowed, err := coord.channelPluginAllowed(t.Context(), &ResolvedChat{
		AgentID: "agent-1",
		ChatCtx: ChatContext{Platform: "feishu"},
	})
	if allowed || !errors.Is(err, resolverErr) {
		t.Fatalf("trusted gate = %v, %v; want resolver failure", allowed, err)
	}
}
