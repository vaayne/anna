package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	cfgstore "github.com/CherryHQ/stella/cmd/stellad/store"
	"github.com/CherryHQ/stella/internal/server"
	"github.com/CherryHQ/stella/internal/skill/policy"
)

var errPolicyStore = errors.New("policy store unavailable")

type failingAgentSkillPolicyStore struct{}

func (failingAgentSkillPolicyStore) ReadAgentSkillPolicy(context.Context, string) (policy.Policy, error) {
	return policy.Policy{}, errPolicyStore
}

func (failingAgentSkillPolicyStore) SetAgentSkillPolicy(context.Context, string, string, bool) (policy.Policy, error) {
	return policy.Policy{}, errPolicyStore
}

// TestAgentSkillActivationAuthorizationAndErrors covers the activation-specific
// PEP boundary. Content permissions intentionally remain stricter: a durable
// creator can toggle admin-installed content, but cannot edit it.
func TestAgentSkillActivationAuthorizationAndErrors(t *testing.T) {
	env := setupAdmin(t)
	_, creatorToken := newNonAdmin(t, env, "activation-creator")
	_, otherToken := newNonAdmin(t, env, "activation-other")
	agentID := createAgentAsUser(t, env, creatorToken, "activation-agent")
	createTestSkill(t, env, "system", "", "", "activation-system")
	createTestSkill(t, env, "system_agent", "", agentID, "activation-system-agent")

	patch := func(token, ref string, enabled bool) *httptest.ResponseRecorder {
		t.Helper()
		return doRequestWithSession(t, env.srv, token, http.MethodPatch,
			"/api/agents/"+agentID+"/skills/"+ref+"/activation", map[string]bool{"enabled": enabled})
	}

	for _, ref := range []string{"system:activation-system", "system_agent:activation-system-agent"} {
		rr := patch(creatorToken, ref, false)
		if rr.Code != http.StatusOK {
			t.Fatalf("creator disable %s: status=%d body=%s", ref, rr.Code, rr.Body.String())
		}
		var activation struct {
			LogicalRef string `json:"logical_ref"`
			Enabled    bool   `json:"enabled"`
		}
		if err := json.Unmarshal(parseResponse(t, rr).Data, &activation); err != nil {
			t.Fatalf("unmarshal activation: %v", err)
		}
		if activation.LogicalRef != ref || activation.Enabled {
			t.Fatalf("activation = %#v, want committed disable for %q", activation, ref)
		}
	}

	// Admin can commit too; a non-creator ordinary user cannot.
	if rr := patch(env.bearerToken, "system:activation-system", true); rr.Code != http.StatusOK {
		t.Fatalf("admin enable: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := patch(otherToken, "system:activation-system", false); rr.Code != http.StatusForbidden {
		t.Fatalf("other user: status=%d want 403 body=%s", rr.Code, rr.Body.String())
	}
	if rr := patch(creatorToken, "user:forbidden", false); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid ref: status=%d want 400 body=%s", rr.Code, rr.Body.String())
	}
	if rr := patch(creatorToken, "builtin:stella", false); rr.Code != http.StatusBadRequest {
		t.Fatalf("builtin disable: status=%d want 400 body=%s", rr.Code, rr.Body.String())
	}
	if rr := patch(creatorToken, "system:not-real", false); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown disable: status=%d want 404 body=%s", rr.Code, rr.Body.String())
	}
	if rr := patch(creatorToken, "system:activation-system", false); rr.Code != http.StatusOK {
		t.Fatalf("creator re-disable: status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Management keeps a disabled winning row visible and exposes contextual
	// activation separately from disable_model_invocation.
	rr := doRequestWithSession(t, env.srv, creatorToken, http.MethodGet, "/api/agents/"+agentID+"/skills", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list contextual skills: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := findSkill(decodeSkillList(t, rr), "activation-system"); got == nil || got["enabled"] != false {
		t.Fatalf("disabled management winner = %#v; want visible enabled=false", got)
	}

	// A stored dangling ref may only be cleared by the explicit enable mutation.
	policyStore := env.store.(*cfgstore.DBStore)
	if _, err := env.db.Exec(context.Background(), `UPDATE agent SET enabled_builtin_skills = '{"version":1,"disabled":["system:removed-skill"]}'::jsonb WHERE id = $1`, agentID); err != nil {
		t.Fatalf("seed dangling policy: %v", err)
	}
	if rr := patch(creatorToken, "system:removed-skill", true); rr.Code != http.StatusOK {
		t.Fatalf("clear dangling: status=%d body=%s", rr.Code, rr.Body.String())
	}
	policy, err := policyStore.ReadAgentSkillPolicy(context.Background(), agentID)
	if err != nil || policy.DisabledRef("system:removed-skill") {
		t.Fatalf("dangling clear policy=%#v err=%v", policy, err)
	}
	var stored struct {
		Version  int       `json:"version"`
		Disabled *[]string `json:"disabled"`
	}
	var raw []byte
	if err := env.db.QueryRow(context.Background(), `SELECT enabled_builtin_skills FROM agent WHERE id = $1`, agentID).Scan(&raw); err != nil || json.Unmarshal(raw, &stored) != nil || stored.Version != 1 || stored.Disabled == nil || len(*stored.Disabled) != 0 {
		t.Fatalf("dangling clear stored policy=%s err=%v; want canonical non-null empty v1", raw, err)
	}

	// The same creator has the activation capability but not the Skill content
	// edit capability for the admin-owned system row.
	rr = doRequestWithSession(t, env.srv, creatorToken, http.MethodPatch,
		"/api/agents/"+agentID+"/skills/activation-system?scope=system", map[string]string{"description": "nope"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("creator edits admin system skill: status=%d want 403 body=%s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithSession(t, env.srv, creatorToken, http.MethodDelete,
		"/api/agents/"+agentID+"/skills/activation-system?scope=system", nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("creator deletes admin system skill: status=%d want 403 body=%s", rr.Code, rr.Body.String())
	}

	// Persistence/catalog failures are internal errors, never an opaque clean
	// not-found response that would invite the caller to overwrite state.
	env.rebuild(t, func(deps *server.Deps) { deps.AgentSkillPolicy = failingAgentSkillPolicyStore{} })
	rr = patch(env.bearerToken, "system:activation-system", false)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("policy store failure: status=%d want 500 body=%s", rr.Code, rr.Body.String())
	}
}

func TestAgentSkillBuiltinExactScopeReadOnly(t *testing.T) {
	env := setupAdmin(t)
	_, token := newNonAdmin(t, env, "builtin-exact")
	agentID := createAgentAsUser(t, env, token, "builtin-exact-agent")

	list := doRequestWithSession(t, env.srv, token, http.MethodGet, "/api/agents/"+agentID+"/skills?scope=builtin", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list builtin skills: status=%d body=%s", list.Code, list.Body.String())
	}
	builtin := findSkill(decodeSkillList(t, list), "stella")
	if builtin == nil {
		t.Fatal("listed builtin stella not found")
	}
	id, ok := builtin["id"].(string)
	if !ok || id == "" {
		t.Fatalf("builtin list row ID=%#v", builtin["id"])
	}
	base := "/api/agents/" + agentID + "/skills/" + id + "?scope=builtin"
	if rr := doRequestWithSession(t, env.srv, token, http.MethodGet, base, nil); rr.Code != http.StatusOK {
		t.Fatalf("exact builtin get: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := doRequestWithSession(t, env.srv, token, http.MethodGet, "/api/agents/"+agentID+"/skills/"+id+"/file?scope=builtin&path=SKILL.md", nil); rr.Code != http.StatusOK {
		t.Fatalf("exact builtin file: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := doRequestWithSession(t, env.srv, token, http.MethodPatch, base, map[string]any{"description": "nope"}); rr.Code != http.StatusForbidden {
		t.Fatalf("builtin update: status=%d want 403 body=%s", rr.Code, rr.Body.String())
	}
	if rr := doRequestWithSession(t, env.srv, token, http.MethodDelete, base, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("builtin delete: status=%d want 403 body=%s", rr.Code, rr.Body.String())
	}

	createTestSkill(t, env, "system", "", "", "stella")
	list = doRequestWithSession(t, env.srv, token, http.MethodGet, "/api/agents/"+agentID+"/skills?scope=builtin", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list shadowed builtin skills: status=%d body=%s", list.Code, list.Body.String())
	}
	if got := findSkill(decodeSkillList(t, list), "stella"); got != nil {
		t.Fatalf("shadowed builtin unexpectedly listed: %#v", got)
	}
	if rr := doRequestWithSession(t, env.srv, token, http.MethodGet, base, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("shadowed builtin exact get: status=%d want 404 body=%s", rr.Code, rr.Body.String())
	}
}

func TestAgentSkillPolicyDiagnosticsKeepShadowedCatalogRefs(t *testing.T) {
	env := setupAdmin(t)
	user, token := newNonAdmin(t, env, "policy-diagnostics")
	agentID := createAgentAsUser(t, env, token, "policy-diagnostics-agent")
	createTestSkill(t, env, "system", "", "", "shadowed-policy-skill")
	createTestSkill(t, env, "user_agent", user.ID, agentID, "shadowed-policy-skill")
	if _, err := env.db.Exec(context.Background(), `UPDATE agent SET enabled_builtin_skills = $1::jsonb WHERE id = $2`, `{"version":1,"disabled":["builtin:stella","system:missing-a","system:missing-b","system:shadowed-policy-skill"]}`, agentID); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	rr := doRequestWithSession(t, env.srv, token, http.MethodGet, "/api/agents/"+agentID+"/skills", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list policy diagnostics: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var response struct {
		PolicyDiagnostics struct {
			Dangling []string `json:"dangling_disabled_refs"`
		} `json:"policy_diagnostics"`
	}
	if err := json.Unmarshal(parseResponse(t, rr).Data, &response); err != nil {
		t.Fatalf("decode policy diagnostics: %v", err)
	}
	if got, want := response.PolicyDiagnostics.Dangling, []string{"system:missing-a", "system:missing-b"}; !slices.Equal(got, want) {
		t.Fatalf("dangling refs=%v, want %v", got, want)
	}
}
