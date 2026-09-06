package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/CherryHQ/stella/internal/authz"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	"github.com/CherryHQ/stella/internal/platform/config"
)

func TestSnapshotAccessDefensivelyCopiesNestedValues(t *testing.T) {
	enabled := true
	def := Definition{
		ID: "builtin/demo", Namespace: "demo", DisplayName: "Demo", Backend: BackendGo,
		Source: SourceBuiltin, ImplementationKey: "demo", Revision: 1,
		Spec: json.RawMessage(`{"base":"definition"}`),
	}
	catalog := NewCatalog()
	if err := catalog.Register(def); err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{catalog: catalog, configs: []Config{{
		ID:             "config-1",
		PluginID:       "builtin/demo",
		Namespace:      "demo",
		Scope:          ScopeSystem,
		Enabled:        &enabled,
		Payload:        json.RawMessage(`{"key":"config"}`),
		CredentialRefs: json.RawMessage(`{"vault":"ref"}`),
		Revision:       1,
	}}}

	got, ok := snapshot.Get("builtin/demo")
	if !ok {
		t.Fatal("Get returned no plugin")
	}
	got.Definition.Spec[0] = 'X'
	got.Config.Payload[0] = 'X'
	got.Config.CredentialRefs[0] = 'X'
	*got.Config.Enabled = false
	got.Effective.Payload[0] = 'X'

	again, ok := snapshot.Get("builtin/demo")
	if !ok {
		t.Fatal("second Get returned no plugin")
	}
	if string(again.Definition.Spec) != `{"base":"definition"}` || string(again.Config.Payload) != `{"key":"config"}` || string(again.Config.CredentialRefs) != `{"vault":"ref"}` || !*again.Config.Enabled || string(again.Effective.Payload) != `{"base":"definition","key":"config"}` {
		t.Fatalf("snapshot was mutated through Get: %#v", again)
	}

	defs := snapshot.Definitions()
	defs[0].Spec[0] = 'Y'
	resolved, err := snapshot.Resolve("builtin/demo")
	if err != nil {
		t.Fatal(err)
	}
	resolved.Payload[0] = 'Z'
	resolvedAgain, err := snapshot.Resolve("builtin/demo")
	if err != nil {
		t.Fatal(err)
	}
	if string(resolvedAgain.Payload) != `{"base":"definition","key":"config"}` {
		t.Fatalf("snapshot was mutated through accessors: %s", resolvedAgain.Payload)
	}
}

func TestSnapshotResolveNamespaceMissingIsNotFound(t *testing.T) {
	_, err := (Snapshot{}).ResolveNamespace("missing")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("missing namespace error = %v", err)
	}
}

func TestCustomVisibleDistinguishesSharedNegativeFromOwnNegative(t *testing.T) {
	falseValue := false
	cases := []struct {
		name                    string
		scope                   Scope
		userID, agentID         string
		configUser, configAgent string
		payload                 json.RawMessage
		want                    bool
	}{
		{name: "shared negative", scope: ScopeSystem, payload: nil, want: false},
		{name: "shared payload", scope: ScopeSystem, payload: json.RawMessage(`{}`), want: true},
		{name: "own user negative", scope: ScopeUser, userID: "u", configUser: "u", want: true},
		{name: "own agent negative", scope: ScopeUserAgent, userID: "u", agentID: "a", configUser: "u", configAgent: "a", want: true},
		{name: "foreign user negative", scope: ScopeUser, userID: "u", configUser: "other", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := Config{Scope: tc.scope, UserID: tc.configUser, AgentID: tc.configAgent, Payload: tc.payload, Enabled: &falseValue}
			if got := customVisible([]Config{config}, tc.userID, tc.agentID); got != tc.want {
				t.Fatalf("customVisible = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSnapshotIdentityUsesTrustedAuthority(t *testing.T) {
	service := &Service{}
	user, err := authz.NewUserAuthority("user-1", false)
	if err != nil {
		t.Fatal(err)
	}
	userID, agentID, empty, err := service.snapshotIdentity(t.Context(), user, "")
	if err != nil || userID != "user-1" || agentID != "" || empty {
		t.Fatalf("user identity = %q/%q/%v, %v", userID, agentID, empty, err)
	}

	guest, err := authz.NewGuestAuthority("guest-1", "channel-1")
	if err != nil {
		t.Fatal(err)
	}
	_, _, empty, err = service.snapshotIdentity(t.Context(), guest, "")
	if err != nil || !empty {
		t.Fatalf("guest identity = empty:%v, err:%v", empty, err)
	}
	if _, _, _, err = service.snapshotIdentity(t.Context(), guest, "agent-1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("guest agent override = %v, want forbidden", err)
	}

	system, err := authz.NewSystemAuthority("maintenance")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = service.snapshotIdentity(t.Context(), system, "agent-1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("system agent override = %v, want forbidden", err)
	}

	delegated, err := authz.NewAgentAuthority("user-1", "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = service.snapshotIdentity(t.Context(), delegated, "other-agent"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("agent switch = %v, want forbidden", err)
	}
	disabledAgents := agentaccess.NewService(disabledSnapshotAgentStore{}, assignedAgentLinks{})
	delegatedDisabled, err := authz.NewAgentAuthority("user-1", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = (&Service{agents: disabledAgents}).snapshotIdentity(t.Context(), delegatedDisabled, ""); !errors.Is(err, agentaccess.ErrForbidden) {
		t.Fatalf("disabled delegated agent = %v, want forbidden", err)
	}
	groupDisabled, err := authz.NewGroupAgentAuthority("group-1", "group-agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = (&Service{agents: disabledAgents}).snapshotIdentity(t.Context(), groupDisabled, ""); !errors.Is(err, agentaccess.ErrForbidden) {
		t.Fatalf("disabled group agent = %v, want forbidden", err)
	}

	disabledUser, err := authz.NewUserAuthority("user-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = (&Service{agents: disabledAgents}).snapshotIdentity(t.Context(), disabledUser, "agent"); !errors.Is(err, agentaccess.ErrForbidden) {
		t.Fatalf("disabled execution agent = %v, want forbidden", err)
	}
}

type disabledSnapshotAgentStore struct{}

func (disabledSnapshotAgentStore) GetAgent(_ context.Context, id string) (config.Agent, error) {
	return config.Agent{ID: id, Scope: config.AgentScopeRestricted, CreatorID: "user-1", Enabled: false}, nil
}

func (disabledSnapshotAgentStore) ListAgents(context.Context) ([]config.Agent, error) {
	return nil, nil
}
