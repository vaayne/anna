package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/CherryHQ/stella/internal/authz"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	"github.com/CherryHQ/stella/internal/platform/config"
)

type assignedAgentStore struct{}

func (assignedAgentStore) GetAgent(context.Context, string) (config.Agent, error) {
	return config.Agent{ID: "agent", Scope: config.AgentScopeRestricted, CreatorID: "user-b", Enabled: true}, nil
}

func (assignedAgentStore) ListAgents(context.Context) ([]config.Agent, error) { return nil, nil }

type assignedAgentLinks struct{}

func (assignedAgentLinks) ListUserAgentIDs(context.Context, string) ([]string, error) {
	return []string{"agent"}, nil
}

func TestAccessAllowsAssignedAgentOwnedByAnotherUser(t *testing.T) {
	authority, err := authz.NewUserAuthority("user-a", false)
	if err != nil {
		t.Fatal(err)
	}
	pep := agentaccess.NewService(assignedAgentStore{}, assignedAgentLinks{})
	access := &Access{service: &Service{agents: pep}, authority: authority}
	userID, agentID, err := access.owner(t.Context(), ScopeUserAgent, "agent")
	if err != nil || userID != "user-a" || agentID != "agent" {
		t.Fatalf("assigned owner = %q/%q, err=%v", userID, agentID, err)
	}
}

func TestAccessRequiresAgentPEPForAgentScopes(t *testing.T) {
	authority, err := authz.NewUserAuthority("user", false)
	if err != nil {
		t.Fatal(err)
	}
	access := &Access{service: &Service{}, authority: authority}
	if _, _, err := access.owner(t.Context(), ScopeUserAgent, "agent"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("user-agent without PEP = %v, want forbidden", err)
	}
}

func TestAccessOwnerAuthorityMatrix(t *testing.T) {
	user, err := authz.NewUserAuthority("user", false)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := authz.NewUserAuthority("admin", true)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := authz.NewGuestAuthority("guest", "channel")
	if err != nil {
		t.Fatal(err)
	}
	pep := agentaccess.NewService(assignedAgentStore{}, assignedAgentLinks{})
	cases := []struct {
		name      string
		authority authz.Authority
		scope     Scope
		agentID   string
		wantOK    bool
	}{
		{"user user", user, ScopeUser, "", true},
		{"admin user", admin, ScopeUser, "", true},
		{"guest user", guest, ScopeUser, "", false},
		{"invalid user", authz.Authority{}, ScopeUser, "", false},
		{"user system", user, ScopeSystem, "", false},
		{"admin system", admin, ScopeSystem, "", true},
		{"guest system", guest, ScopeSystem, "", false},
		{"invalid system", authz.Authority{}, ScopeSystem, "", false},
		{"user system-agent", user, ScopeSystemAgent, "agent", false},
		{"admin system-agent", admin, ScopeSystemAgent, "agent", true},
		{"guest system-agent", guest, ScopeSystemAgent, "agent", false},
		{"invalid system-agent", authz.Authority{}, ScopeSystemAgent, "agent", false},
		{"user user-agent", user, ScopeUserAgent, "agent", true},
		{"admin user-agent", admin, ScopeUserAgent, "agent", true},
		{"guest user-agent", guest, ScopeUserAgent, "agent", false},
		{"invalid user-agent", authz.Authority{}, ScopeUserAgent, "agent", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			access := &Access{service: &Service{agents: pep}, authority: tc.authority}
			userID, agentID, err := access.owner(t.Context(), tc.scope, tc.agentID)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("owner error = %v", err)
				}
				if tc.scope == ScopeUser && userID == "" {
					t.Fatal("user scope returned empty user owner")
				}
				if tc.scope == ScopeSystem && (userID != "" || agentID != "") {
					t.Fatalf("system owner = %q/%q", userID, agentID)
				}
				return
			}
			if err == nil {
				t.Fatalf("owner = %q/%q, want rejection", userID, agentID)
			}
		})
	}
}

func TestPatchPayloadPreservesOmittedFieldsAndExplicitResets(t *testing.T) {
	current := json.RawMessage(`{"version":"1","env":{"keep":true},"owned":"secret"}`)
	updated, err := patchPayload(current, ConfigPatch{
		PayloadSet: true,
		Payload:    json.RawMessage(`{"version":"2"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(updated, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["version"]) != `"2"` || string(fields["env"]) != `{"keep":true}` || string(fields["owned"]) != `"secret"` {
		t.Fatalf("omitted patch fields changed: %s", updated)
	}

	updated, err = patchPayload(updated, ConfigPatch{ResetFields: []string{"owned"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(updated) != `{"env":{"keep":true},"version":"2"}` && string(updated) != `{"version":"2","env":{"keep":true}}` {
		t.Fatalf("reset field retained: %s", updated)
	}

	updated, err = patchPayload(updated, ConfigPatch{
		PayloadSet: true,
		Payload:    json.RawMessage(`{"owned":null}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(updated) != `{"env":{"keep":true},"owned":null,"version":"2"}` && string(updated) != `{"version":"2","env":{"keep":true},"owned":null}` && string(updated) != `{"env":{"keep":true},"version":"2","owned":null}` {
		t.Fatalf("explicit null ownership was lost: %s", updated)
	}
}

func TestApplyCLIWriteOnlyPatchMaterializesOnlyKnownResources(t *testing.T) {
	definition := Definition{
		Backend: BackendCLI,
		Spec:    []byte(`{"binaries":[{"name":"tool","tool":"github:owner/tool","version":"1.0","options":{"channel":"stable"}}],"skills":[{"name":"docs"}]}`),
	}
	patched, err := applyCLIWriteOnlyPatch(definition, []byte(`{"binaries":[{"name":"tool","version":"1.5"}]}`), ConfigPatch{
		BinaryVersionsSet: true,
		BinaryVersions:    map[string]string{"tool": "2.0"},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string][]map[string]any
	if err := json.Unmarshal(patched, &payload); err != nil {
		t.Fatal(err)
	}
	if got := payload["binaries"][0]["version"]; got != "2.0" {
		t.Fatalf("version = %#v, want 2.0", got)
	}
	if got := payload["binaries"][0]["tool"]; got != "github:owner/tool" {
		t.Fatalf("tool = %#v, want definition locator", got)
	}
	if _, ok := payload["skills"]; ok {
		t.Fatalf("write-only patch unexpectedly materialized mutable skills: %#v", payload["skills"])
	}
}

func TestApplyCLIWriteOnlyPatchRejectsUnknownAndUnauthorizedResources(t *testing.T) {
	definition := Definition{Backend: BackendCLI, Spec: []byte(`{"binaries":[{"name":"tool","tool":"uv","version":"1"}],"skills":[{"name":"docs"}]}`)}
	_, err := applyCLIWriteOnlyPatch(definition, nil, ConfigPatch{
		BinaryVersionsSet: true,
		BinaryVersions:    map[string]string{"missing": "2"},
	}, false)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("unknown binary error = %v, want invalid config", err)
	}
}

func TestRejectImmutableSkillPatch(t *testing.T) {
	for _, test := range []struct {
		name  string
		patch ConfigPatch
		want  bool
	}{
		{name: "malformed payload", patch: ConfigPatch{PayloadSet: true, Payload: json.RawMessage(`{"skills":`)}, want: true},
		{name: "non-object payload", patch: ConfigPatch{PayloadSet: true, Payload: json.RawMessage(`[]`)}, want: true},
		{name: "null payload", patch: ConfigPatch{PayloadSet: true, Payload: json.RawMessage(`null`)}, want: true},
		{name: "null skill", patch: ConfigPatch{PayloadSet: true, Payload: json.RawMessage(`{"skills":null}`)}, want: true},
		{name: "same skill set", patch: ConfigPatch{PayloadSet: true, Payload: json.RawMessage(`{"skills":[{"name":"docs"}]}`)}, want: true},
		{name: "reset skill", patch: ConfigPatch{ResetFields: []string{"skills"}}, want: true},
		{name: "ordinary field", patch: ConfigPatch{PayloadSet: true, Payload: json.RawMessage(`{"version":"2"}`)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := rejectImmutableSkillPatch(test.patch)
			if (err != nil) != test.want {
				t.Fatalf("rejectImmutableSkillPatch() error = %v, want error %v", err, test.want)
			}
		})
	}
}

func TestValidateCustomSpecRejectsClaimedSkills(t *testing.T) {
	base := Definition{
		ID: "custom/demo", Namespace: "demo", DisplayName: "Demo",
		Backend: BackendCLI, Source: SourceCustom, ImplementationKey: "cli", Revision: 1,
	}
	for _, test := range []struct {
		name string
		spec string
		want bool
	}{
		{name: "nonempty", spec: `{"skills":[{"name":"docs"}]}`, want: true},
		{name: "empty", spec: `{"skills":[]}`},
		{name: "null", spec: `{"skills":null}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			definition := base
			definition.Spec = json.RawMessage(test.spec)
			err := validateCustomSpec(definition)
			if (err != nil) != test.want {
				t.Fatalf("validateCustomSpec() error = %v, want error %v", err, test.want)
			}
		})
	}
}
