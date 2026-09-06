package server_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CherryHQ/stella/internal/auth"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/skill"
)

// newNonAdmin creates a non-admin user with a bearer token and returns
// (user, bearer token).
func newNonAdmin(t *testing.T, env *testEnv, username string) (auth.User, string) {
	t.Helper()
	return createTestUserWithToken(t, env.authStore, env.oidcStore, username, auth.RoleUser)
}

// createAgentAsUser creates an agent via the API using the given session
// (so CreatorID is set to that session's user). Returns the agent ID.
func createAgentAsUser(t *testing.T, env *testEnv, sessionID, name string) string {
	t.Helper()
	rr := doRequestWithSession(t, env.srv, sessionID, "POST", "/api/agents", config.Agent{
		Name:    name,
		Model:   "anthropic/claude-sonnet-4-6",
		Enabled: true,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create agent %q: status = %d (body: %s)", name, rr.Code, rr.Body.String())
	}
	resp := parseResponse(t, rr)
	var created config.Agent
	if err := json.Unmarshal(resp.Data, &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return created.ID
}

// createTestSkill creates a skill via the store and returns its ID.
func createTestSkill(t *testing.T, env *testEnv, scope string, userID string, agentID, name string) string {
	t.Helper()
	sk := skill.Skill{
		Scope:       scope,
		UserID:      userID,
		AgentID:     agentID,
		Name:        name,
		Description: "test",
		Status:      "active",
	}
	ctx := context.Background()
	snapshot, err := env.skillStore.CreateManagedSkill(ctx, sk, map[string]string{
		skill.MainFile: "# " + name,
		"reference.md": "reference content",
	})
	if err != nil {
		t.Fatalf("Create skill: %v", err)
	}
	return snapshot.Skill.ID
}

func currentSkillDigest(t *testing.T, env *testEnv, id string) string {
	t.Helper()
	identity, err := env.skillStore.GetIdentity(t.Context(), id)
	if err != nil || identity == nil {
		t.Fatalf("load Skill identity %s: %#v, %v", id, identity, err)
	}
	revision, err := env.skillStore.LoadCurrentRevision(t.Context(), *identity)
	if err != nil {
		t.Fatalf("load current Skill revision %s: %v", id, err)
	}
	return revision.Skill.ContentDigest
}

func responseSkillDigest(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var got struct {
		ContentDigest string `json:"content_digest"`
	}
	if err := json.Unmarshal(parseResponse(t, rr).Data, &got); err != nil {
		t.Fatalf("unmarshal Skill mutation digest: %v", err)
	}
	if got.ContentDigest == "" {
		t.Fatal("Skill mutation response omitted content_digest")
	}
	return got.ContentDigest
}

func createSkillZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func doMultipartRequestWithSession(t *testing.T, srv http.Handler, bearerToken, method, path, fieldName, fileName string, fileData []byte, extraFields ...string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for i := 0; i+1 < len(extraFields); i += 2 {
		if err := mw.WriteField(extraFields[i], extraFields[i+1]); err != nil {
			t.Fatalf("write field %q: %v", extraFields[i], err)
		}
	}
	part, err := mw.CreateFormFile(fieldName, fileName)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(fileData); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(method, path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if bearerToken != "" {
		if strings.HasPrefix(bearerToken, "stella_") {
			req.Header.Set("Authorization", "Bearer "+bearerToken)
		} else {
			req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: bearerToken})
		}
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func decodeSkillList(t *testing.T, rr *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var list []map[string]any
	if err := json.Unmarshal(parseListItems(t, rr, "skills"), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	return list
}

func findSkill(list []map[string]any, name string) map[string]any {
	for _, item := range list {
		if item["name"] == name {
			return item
		}
	}
	return nil
}

// TestAgentSkills_ListPrecedenceDedup guards CR-004: when the same name exists
// at multiple scopes, the agent listing must show the effective (highest
// precedence) row, not whichever scope sorts first alphabetically.
func TestAgentSkills_ListPrecedenceDedup(t *testing.T) {
	env := setupAdmin(t)
	user, sid := newNonAdmin(t, env, "list-precedence")
	agentID := createAgentAsUser(t, env, sid, "list-precedence-agent")

	// Same name at system_agent (admin/shared) and user_agent (personal override).
	createTestSkill(t, env, "system_agent", "", agentID, "dup")
	createTestSkill(t, env, "user_agent", user.ID, agentID, "dup")

	rr := doRequestWithSession(t, env.srv, sid, "GET", "/api/agents/"+agentID+"/skills", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	list := decodeSkillList(t, rr)
	dup := findSkill(list, "dup")
	if dup == nil {
		t.Fatalf("dup skill missing from list: %#v", list)
	}
	if dup["scope"] != "user_agent" {
		t.Fatalf("dup scope = %v, want user_agent (the higher-precedence override)", dup["scope"])
	}
}

// TestAgentSkills_RouteAgentGateAppliesToUserScope proves the route agent's read
// gate is folded into the single Skill evaluation for every scope: a caller who
// cannot read the route agent is denied a user-scoped DB skill reached through
// that agent, even though the skill's own row is not agent-bound.
func TestAgentSkills_RouteAgentGateAppliesToUserScope(t *testing.T) {
	env := setupAdmin(t)
	creator, creatorSID := newNonAdmin(t, env, "route-gate-creator")
	_, otherSID := newNonAdmin(t, env, "route-gate-other")
	agentID := createAgentAsUser(t, env, creatorSID, "route-gate-agent")
	createTestSkill(t, env, "user", creator.ID, "", "user-scope-skill")

	// The creator can read the route agent and their own user-scoped skill.
	rr := doRequestWithSession(t, env.srv, creatorSID, "GET", "/api/agents/"+agentID+"/skills/user-scope-skill?scope=user", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("creator get status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}

	// A caller with no access to the route agent is denied, gated by the folded
	// agent-read decision even though the target skill's scope is not agent-bound.
	rr = doRequestWithSession(t, env.srv, otherSID, "GET", "/api/agents/"+agentID+"/skills/user-scope-skill?scope=user", nil)
	if rr.Code != http.StatusForbidden && rr.Code != http.StatusNotFound {
		t.Fatalf("other get status = %d, want 403/404 (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestSessionSystemPromptAdvertisesSkillSearch(t *testing.T) {
	env := setupAdmin(t)
	agentID := createAgentAsUser(t, env, env.bearerToken, "prompt-skills-agent")
	createTestSkill(t, env, "system", "", "", "inspect-skill")

	sessionID := "prompt-skills-session"
	sm := env.mem.(memory.SessionManager)
	now := time.Now()
	if err := sm.SaveInfo(context.Background(), memory.SessionInfo{
		ID:         sessionID,
		AgentID:    agentID,
		UserID:     env.adminUser.ID,
		Channel:    "admin",
		Kind:       "chat",
		CreatedAt:  now,
		LastActive: now,
	}); err != nil {
		t.Fatalf("SaveInfo: %v", err)
	}

	rr := doRequest(t, env, "GET", "/api/agents/"+agentID+"/sessions/"+sessionID+"/system-prompt", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	resp := parseResponse(t, rr)
	var got struct {
		SystemPrompt string `json:"system_prompt"`
	}
	if err := json.Unmarshal(resp.Data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, want := range []string{"## Skills", "skill_installed_search", `skill_load: name="<skill-name>"`} {
		if !strings.Contains(got.SystemPrompt, want) {
			t.Fatalf("system prompt missing %q:\n%s", want, got.SystemPrompt)
		}
	}
	// System skills stay visible; project/user/agent skills are discovered through skill_installed_search.
	if !strings.Contains(got.SystemPrompt, "<name>inspect-skill</name>") {
		t.Fatalf("system prompt should list system skill names:\n%s", got.SystemPrompt)
	}
}

// --- Agent-context skill endpoints ---

func TestAgentSkills_ListVisibleSkills(t *testing.T) {
	env := setupAdmin(t)

	creator, creatorSID := newNonAdmin(t, env, "creator-list")
	_, otherSID := newNonAdmin(t, env, "other-list")

	agentID := createAgentAsUser(t, env, creatorSID, "list-agent")
	createTestSkill(t, env, "system", "", "", "system-skill")
	createTestSkill(t, env, "system_agent", "", agentID, "agent-skill")
	createTestSkill(t, env, "user_agent", creator.ID, agentID, "creator-user-skill")
	deprecatedID := createTestSkill(t, env, "user_agent", creator.ID, agentID, "deprecated-skill")
	deprecated := skill.SkillStatusDeprecated
	if _, err := env.skillStore.UpdateManagedSkill(t.Context(), skill.ManagedSkillUpdate{
		ID: deprecatedID, UserID: creator.ID, AgentID: agentID, Scope: "user_agent",
		Patch: skill.UpdatePatch{Status: &deprecated}, ExpectedDigest: currentSkillDigest(t, env, deprecatedID),
	}); err != nil {
		t.Fatalf("deprecate Skill through managed authority: %v", err)
	}

	rr := doRequestWithSession(t, env.srv, creatorSID, "GET", "/api/agents/"+agentID+"/skills", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("creator status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	list := decodeSkillList(t, rr)
	for _, name := range []string{"system-skill", "agent-skill", "creator-user-skill"} {
		if findSkill(list, name) == nil {
			t.Fatalf("creator list missing %q: %#v", name, list)
		}
	}
	if deprecated := findSkill(list, "deprecated-skill"); deprecated != nil {
		t.Fatalf("creator list included deprecated skill: %#v", deprecated)
	}

	rr = doRequest(t, env, "GET", "/api/agents/"+agentID+"/skills", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	list = decodeSkillList(t, rr)
	if findSkill(list, "system-skill") == nil || findSkill(list, "agent-skill") == nil {
		t.Fatalf("admin list missing system or agent skill: %#v", list)
	}
	if findSkill(list, "creator-user-skill") != nil {
		t.Fatalf("admin list included another user's skill: %#v", list)
	}

	// Non-admin users without agent assignment are denied access.
	rr = doRequestWithSession(t, env.srv, otherSID, "GET", "/api/agents/"+agentID+"/skills", nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("other status = %d, want 403 (body: %s)", rr.Code, rr.Body.String())
	}

	rr = doUnauthRequest(t, env.srv, "GET", "/api/agents/"+agentID+"/skills", nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status = %d, want 401", rr.Code)
	}
}

func TestAgentSkills_CrossAgentScope(t *testing.T) {
	env := setupAdmin(t)

	_, sid := newNonAdmin(t, env, "creator-cross")
	a1 := createAgentAsUser(t, env, sid, "cross-a1")
	a2 := createAgentAsUser(t, env, sid, "cross-a2")

	createTestSkill(t, env, "system_agent", "", a1, "skill-on-agent1")

	// skill-on-agent1 belongs to a1, so fetching via a2 should 404
	rr := doRequestWithSession(t, env.srv, sid, "GET", "/api/agents/"+a2+"/skills/skill-on-agent1", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-agent get status = %d, want 404 (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestAgentSkills_CreateUpdateDeleteFile(t *testing.T) {
	env := setupAdmin(t)

	creator, sid := newNonAdmin(t, env, "creator-ud")
	_, otherSID := newNonAdmin(t, env, "other-ud")
	agentID := createAgentAsUser(t, env, sid, "ud-agent")

	// Non-admin users without agent assignment are denied access.
	rr := doRequestWithSession(t, env.srv, otherSID, "POST", "/api/agents/"+agentID+"/skills", map[string]any{
		"name":  "other-agent-skill",
		"scope": "system_agent",
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("other create status = %d, want 403 (body: %s)", rr.Code, rr.Body.String())
	}

	// Cannot create system-scoped skills
	rr = doRequestWithSession(t, env.srv, sid, "POST", "/api/agents/"+agentID+"/skills", map[string]any{
		"name":  "system-skill",
		"scope": "system",
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("system create status = %d, want 403 (body: %s)", rr.Code, rr.Body.String())
	}

	// Creator can create user_agent-scoped skill (personal, bound to this agent)
	rr = doRequestWithSession(t, env.srv, sid, "POST", "/api/agents/"+agentID+"/skills", map[string]any{
		"name":        "user-skill",
		"scope":       "user_agent",
		"description": "personal",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("user create status = %d, want 201 (body: %s)", rr.Code, rr.Body.String())
	}
	assertFullSkillMutationResponse(t, rr, "", "manual")
	listRR := doRequestWithSession(t, env.srv, sid, "GET", "/api/agents/"+agentID+"/skills", nil)
	userSkill := findSkill(decodeSkillList(t, listRR), "user-skill")
	if userSkill == nil || userSkill["scope"] != "user_agent" || userSkill["user_id"] != creator.ID || userSkill["agent_id"] != agentID {
		t.Fatalf("created user skill = %#v, want user_agent scoped to creator and agent", userSkill)
	}

	// system_agent is admin-only: the agent creator (non-admin) cannot write it.
	rr = doRequestWithSession(t, env.srv, sid, "POST", "/api/agents/"+agentID+"/skills", map[string]any{
		"name":  "creator-system-agent-skill",
		"scope": "system_agent",
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("creator system_agent create status = %d, want 403 (body: %s)", rr.Code, rr.Body.String())
	}

	// Create an agent-scoped skill for update/delete tests; admins manage it.
	updatedID := createTestSkill(t, env, "system_agent", "", agentID, "skill-ud")
	updatedDigest := currentSkillDigest(t, env, updatedID)

	rr = doRequest(t, env, "PATCH", "/api/agents/"+agentID+"/skills/skill-ud?scope=system_agent", map[string]any{
		"description":     "updated",
		"files":           map[string]string{"SKILL.md": "# updated body"},
		"expected_digest": updatedDigest,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("admin update status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	updatedDigest = responseSkillDigest(t, rr)

	// Non-admin creator cannot update the admin-managed system_agent skill.
	rr = doRequestWithSession(t, env.srv, sid, "PATCH", "/api/agents/"+agentID+"/skills/skill-ud?scope=system_agent", map[string]any{
		"description":     "creator edit",
		"expected_digest": updatedDigest,
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("creator system_agent update status = %d, want 403 (body: %s)", rr.Code, rr.Body.String())
	}

	rr = doRequest(t, env, "DELETE", "/api/agents/"+agentID+"/skills/skill-ud/file?scope=system_agent&path=reference.md&expected_digest="+updatedDigest, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("admin delete file status = %d, want 204 (body: %s)", rr.Code, rr.Body.String())
	}

	rr = doRequest(t, env, "DELETE", "/api/agents/"+agentID+"/skills/skill-ud/file?scope=system_agent&path=SKILL.md&expected_digest="+currentSkillDigest(t, env, updatedID), nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("admin delete SKILL.md status = %d, want 400", rr.Code)
	}
}

// TestAgentSkills_DeleteRemovesUserAgentSkill verifies agent deletion removes
// the database row and its file mirror permanently.
func TestAgentSkills_DeleteRemovesUserAgentSkill(t *testing.T) {
	env := setupAdmin(t)
	user, sid := newNonAdmin(t, env, "agent-delete-lifecycle")
	agentID := createAgentAsUser(t, env, sid, "agent-delete-lifecycle-agent")
	id := createTestSkill(t, env, "user_agent", user.ID, agentID, "agent-delete-lifecycle-skill")

	rr := doRequestWithSession(t, env.srv, sid, http.MethodDelete, "/api/agents/"+agentID+"/skills/agent-delete-lifecycle-skill?scope=user_agent&expected_digest="+currentSkillDigest(t, env, id), nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204 (body: %s)", rr.Code, rr.Body.String())
	}

	var skillCount, fileCount int
	if err := env.db.QueryRow(t.Context(), `SELECT count(*) FROM skill WHERE id = $1`, id).Scan(&skillCount); err != nil {
		t.Fatalf("count deleted agent skill: %v", err)
	}
	if err := env.db.QueryRow(t.Context(), `SELECT count(*) FROM skill_file WHERE skill_id = $1`, id).Scan(&fileCount); err != nil {
		t.Fatalf("count deleted agent files: %v", err)
	}
	if skillCount != 0 || fileCount != 0 {
		t.Fatalf("deleted agent skill retained skill=%d files=%d", skillCount, fileCount)
	}
}

func TestAgentSkills_InstallScopedSkill(t *testing.T) {
	env := setupAdmin(t)

	_, creatorSID := newNonAdmin(t, env, "creator-install")
	agentID := createAgentAsUser(t, env, creatorSID, "install-agent")
	source, err := filepath.Abs("../../plugins/core/skills/stella")
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	// Non-admin creator cannot install into the admin-only system_agent scope.
	rr := doRequestWithSession(t, env.srv, creatorSID, "POST", "/api/agents/"+agentID+"/skills/install", map[string]any{
		"source": source,
		"scope":  "system_agent",
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("creator system_agent install status = %d, want 403 (body: %s)", rr.Code, rr.Body.String())
	}

	// Creator can install into their own per-agent scope.
	rr = doRequestWithSession(t, env.srv, creatorSID, "POST", "/api/agents/"+agentID+"/skills/install", map[string]any{
		"source": source,
		"scope":  "user_agent",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("creator user_agent install status = %d, want 201 (body: %s)", rr.Code, rr.Body.String())
	}
	assertFullSkillMutationResponse(t, rr, "", "manual")

	// Admin can install into the system_agent scope.
	rr = doRequest(t, env, "POST", "/api/agents/"+agentID+"/skills/install", map[string]any{
		"source": source,
		"scope":  "system_agent",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("admin system_agent install status = %d, want 201 (body: %s)", rr.Code, rr.Body.String())
	}
	assertFullSkillMutationResponse(t, rr, "", "manual")

	// Cannot install as system scope
	rr = doRequestWithSession(t, env.srv, creatorSID, "POST", "/api/agents/"+agentID+"/skills/install", map[string]any{
		"source": source,
		"scope":  "system",
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("system install status = %d, want 403 (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestAgentSkills_UploadZip(t *testing.T) {
	env := setupAdmin(t)

	creator, creatorSID := newNonAdmin(t, env, "creator-upload-agent")
	agentID := createAgentAsUser(t, env, creatorSID, "upload-agent")
	// Legacy status frontmatter is ignored; model availability remains independent.
	archive := createSkillZip(t, map[string]string{
		"bundle/uploaded-skill/SKILL.md":     "---\nname: uploaded-skill\ndescription: Uploaded user skill\nstatus: draft\ndisable-model-invocation: true\n---\n# Uploaded\n",
		"bundle/uploaded-skill/reference.md": "notes",
	})

	rr := doMultipartRequestWithSession(t, env.srv.Handler(), creatorSID, "POST", "/api/agents/"+agentID+"/skills/upload", "file", "uploaded-skill.zip", archive, "scope", "user_agent")
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201 (body: %s)", rr.Code, rr.Body.String())
	}
	assertFullSkillMutationResponse(t, rr, "", "manual")

	rr = doRequestWithSession(t, env.srv, creatorSID, "GET", "/api/agents/"+agentID+"/skills", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list uploaded skills status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	uploaded := findSkill(decodeSkillList(t, rr), "uploaded-skill")
	if uploaded == nil {
		t.Fatalf("uploaded skill missing from list")
	}
	if uploaded["scope"] != "user_agent" || uploaded["user_id"] != creator.ID || uploaded["agent_id"] != agentID {
		t.Fatalf("uploaded skill ownership = %#v, want user_agent scoped to creator and agent", uploaded)
	}
	var storedStatus string
	if err := env.db.QueryRow(context.Background(), `SELECT status FROM skill WHERE name = 'uploaded-skill' AND user_id = $1 AND agent_id = $2`, creator.ID, agentID).Scan(&storedStatus); err != nil {
		t.Fatalf("read uploaded skill status: %v", err)
	}
	if storedStatus != "active" {
		t.Fatalf("uploaded stored status = %v, want active", storedStatus)
	}
	if uploaded["disable_model_invocation"] != true {
		t.Fatalf("uploaded disable_model_invocation = %v, want true", uploaded["disable_model_invocation"])
	}
}

func TestAgentUserSkills_SelfOnly(t *testing.T) {
	env := setupAdmin(t)

	u1, sid1 := newNonAdmin(t, env, "user1")
	u2, sid2 := newNonAdmin(t, env, "user2")
	agentID := createAgentAsUser(t, env, sid1, "shared-user-skill-agent")
	if err := env.authStore.AssignAgent(context.Background(), u2.ID, agentID); err != nil {
		t.Fatalf("assign user2 to agent: %v", err)
	}

	createTestSkill(t, env, "user_agent", u1.ID, agentID, "u1-skill")
	createTestSkill(t, env, "user_agent", u2.ID, agentID, "u2-skill")

	rr := doRequestWithSession(t, env.srv, sid1, "GET", "/api/agents/"+agentID+"/skills", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("u1 list status = %d, want 200", rr.Code)
	}
	list := decodeSkillList(t, rr)
	if findSkill(list, "u1-skill") == nil {
		t.Fatalf("u1 list missing own skill: %#v", list)
	}
	if findSkill(list, "u2-skill") != nil {
		t.Fatalf("u1 list included u2 skill: %#v", list)
	}

	// u2 cannot access u1's skill by name even with scope
	rr = doRequestWithSession(t, env.srv, sid2, "GET", "/api/agents/"+agentID+"/skills/u1-skill?scope=user_agent", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("u2 cross status = %d, want 404", rr.Code)
	}

	rr = doRequestWithSession(t, env.srv, sid2, "DELETE", "/api/agents/"+agentID+"/skills/u1-skill?scope=user_agent", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("u2 cross delete status = %d, want 404", rr.Code)
	}
}

func TestAgentSkills_UploadZipRejectsInvalidExtension(t *testing.T) {
	env := setupAdmin(t)
	_, sid := newNonAdmin(t, env, "user-upload-ext")
	agentID := createAgentAsUser(t, env, sid, "upload-ext-agent")
	archive := createSkillZip(t, map[string]string{
		"wrapper/uploaded-skill/SKILL.md": "---\nname: uploaded-skill\ndescription: Uploaded profile skill\n---\n# Uploaded\n",
	})

	rr := doMultipartRequestWithSession(t, env.srv.Handler(), sid, "POST", "/api/agents/"+agentID+"/skills/upload", "file", "uploaded-skill.tar", archive)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("upload status = %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestAgentSkills_UploadZipRejectsMissingSkillMD(t *testing.T) {
	env := setupAdmin(t)
	_, sid := newNonAdmin(t, env, "user-upload-missing")
	agentID := createAgentAsUser(t, env, sid, "upload-missing-agent")
	archive := createSkillZip(t, map[string]string{
		"wrapper/uploaded-skill/reference.md": "reference",
	})

	rr := doMultipartRequestWithSession(t, env.srv.Handler(), sid, "POST", "/api/agents/"+agentID+"/skills/upload", "file", "uploaded-skill.zip", archive)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("upload status = %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestAgentSkills_UploadZipRejectsMultipleSkillRoots(t *testing.T) {
	env := setupAdmin(t)
	_, sid := newNonAdmin(t, env, "user-upload-layout")
	agentID := createAgentAsUser(t, env, sid, "upload-layout-agent")
	archive := createSkillZip(t, map[string]string{
		"wrapper/skill-one/SKILL.md": "---\nname: skill-one\ndescription: one\n---\n# One\n",
		"wrapper/skill-two/SKILL.md": "---\nname: skill-two\ndescription: two\n---\n# Two\n",
	})

	rr := doMultipartRequestWithSession(t, env.srv.Handler(), sid, "POST", "/api/agents/"+agentID+"/skills/upload", "file", "multi.zip", archive)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("upload status = %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestAgentSkills_UploadZipRejectsPathTraversal(t *testing.T) {
	env := setupAdmin(t)
	_, sid := newNonAdmin(t, env, "user-upload-traversal")
	agentID := createAgentAsUser(t, env, sid, "upload-traversal-agent")
	archive := createSkillZip(t, map[string]string{
		"../uploaded-skill/SKILL.md": "---\nname: uploaded-skill\ndescription: Uploaded profile skill\n---\n# Uploaded\n",
	})

	rr := doMultipartRequestWithSession(t, env.srv.Handler(), sid, "POST", "/api/agents/"+agentID+"/skills/upload", "file", "uploaded-skill.zip", archive)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("upload status = %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}
}
