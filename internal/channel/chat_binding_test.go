package channel

import (
	"context"
	"testing"
	"time"

	"github.com/CherryHQ/stella/internal/agent"
	agentruntime "github.com/CherryHQ/stella/internal/agent/runtime"
	"github.com/CherryHQ/stella/internal/agent/session"
	"github.com/CherryHQ/stella/internal/auth"
	"github.com/CherryHQ/stella/internal/core/agentctx"
	"github.com/CherryHQ/stella/internal/memory/memorytest"
	"github.com/CherryHQ/stella/pkg/ai"
)

// bindingProbeRunner records the context a turn runs under, which is the context
// its tools execute in.
type bindingProbeRunner struct{ seen *context.Context }

func (r bindingProbeRunner) Chat(ctx context.Context, _ []ai.Message, _ agentruntime.MessageContent) <-chan agentruntime.Event {
	*r.seen = ctx
	ch := make(chan agentruntime.Event)
	close(ch)
	return ch
}

func (r bindingProbeRunner) Alive() bool             { return true }
func (r bindingProbeRunner) Busy() bool              { return false }
func (r bindingProbeRunner) LastActivity() time.Time { return time.Now() }
func (r bindingProbeRunner) SystemPrompt() string    { return "" }
func (r bindingProbeRunner) PluginContext() agentruntime.PluginContext {
	return agentruntime.PluginContext{}
}
func (r bindingProbeRunner) Close() error { return nil }

// newBindingProbeChat is newCompactTestChat with a runner that captures the turn
// context instead of a nil one.
func newBindingProbeChat(t *testing.T, groupID string, user auth.User, seen *context.Context) *ResolvedChat {
	t.Helper()
	const agentID = "cmd-agent"
	fake := memorytest.New()
	reg, err := session.NewRegistry(fake, agentID)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	rt, err := agentruntime.New(agentruntime.Config{
		Memory: fake,
		NewRunner: func(context.Context, agentruntime.RunnerParams) (agentruntime.Runner, error) {
			return bindingProbeRunner{seen: seen}, nil
		},
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	svc := &agent.Service{Sessions: reg, Runtime: rt, SessionAccess: compactSessionAccessSvc{reg: reg}, AgentID: agentID}
	rc := &ResolvedChat{Service: svc, AgentID: agentID, User: user, GroupID: groupID, Authority: mustCompactAuthority(t, groupID, agentID, user)}
	if groupID != "" {
		rc.SessionKey = agent.BuildGroupSessionKey(agentID, groupID)
		rc.Channel = session.Channel("group:" + groupID)
	} else {
		rc.SessionKey = agent.BuildUserSessionKey(agentID, user.ID, "private")
		rc.Channel = session.Channel(rc.SessionKey)
	}
	return rc
}

func runProbeTurn(t *testing.T, rc *ResolvedChat) {
	t.Helper()
	stream, _, err := rc.Chat(context.Background(), "hello")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	for evt := range stream {
		if evt.Err != nil {
			t.Fatalf("chat event: %v", evt.Err)
		}
	}
}

// TestChatTurnCarriesDurableChatBinding proves the marker that gates
// chat-only session work actually reaches a running turn. Without it a real
// chat would fail closed; with it attached anywhere else, a Web tab could reset
// a session out from under itself.
func TestChatTurnCarriesDurableChatBinding(t *testing.T) {
	t.Run("dm binds to the main session", func(t *testing.T) {
		var seen context.Context
		rc := newBindingProbeChat(t, "", auth.User{ID: "user-1", Role: auth.RoleUser}, &seen)
		runProbeTurn(t, rc)

		binding, ok := agentctx.ChatBindingFromContext(seen)
		if !ok {
			t.Fatal("a DM turn must carry its durable chat binding")
		}
		if !binding.Main {
			t.Fatalf("binding = %+v, want the main-session shape", binding)
		}
		if agentctx.TurnIDFromContext(seen) == "" {
			t.Fatal("a turn must carry an id that distinguishes it from the next one")
		}
	})

	t.Run("group binds to the channel", func(t *testing.T) {
		const groupID = "11111111-1111-4111-8111-111111111111"
		var seen context.Context
		rc := newBindingProbeChat(t, groupID, auth.User{}, &seen)
		runProbeTurn(t, rc)

		binding, ok := agentctx.ChatBindingFromContext(seen)
		if !ok {
			t.Fatal("a group turn must carry its durable chat binding")
		}
		if binding.Main {
			t.Fatalf("binding = %+v, want the channel shape", binding)
		}
		if binding.SessionKey != agent.BuildGroupSessionKey(rc.AgentID, groupID) {
			t.Fatalf("binding session key = %q", binding.SessionKey)
		}
	})
}
