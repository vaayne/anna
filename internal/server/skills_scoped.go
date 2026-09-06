package server

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"sort"
	"strings"

	apiserver "github.com/CherryHQ/stella/api/server"
	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/skill"
	"github.com/CherryHQ/stella/internal/skill/access"
)

// beginSkillAccess opens one Skill policy evaluation for an authenticated caller.
// The Authority carries the verified session role; request path/body fields never
// contribute to it. Every DB-backed skill decision flows through the returned
// Access, so collection and per-row visibility cannot drift.
func (s *Server) beginSkillAccess(ctx context.Context) (*access.Access, int, string) {
	if s.skillAccess == nil {
		return nil, http.StatusServiceUnavailable, "skills authorization unavailable"
	}
	info := UserFromContext(ctx)
	if info == nil {
		return nil, http.StatusUnauthorized, "unauthorized"
	}
	authority, err := info.authority()
	if err != nil {
		return nil, http.StatusForbidden, "forbidden"
	}
	acc, err := s.skillAccess.Begin(ctx, authority)
	if err != nil {
		code, msg := skillAccessError(err)
		return nil, code, msg
	}
	return acc, 0, ""
}

// skillAccessError maps a Skill PEP sentinel to an HTTP status and message,
// preserving the accepted 404-not-found / 403-forbidden split.
func skillAccessError(err error) (int, string) {
	switch {
	case err == nil:
		return 0, ""
	case errors.Is(err, access.ErrNotFound):
		return http.StatusNotFound, "skill not found"
	case errors.Is(err, access.ErrForbidden):
		return http.StatusForbidden, "forbidden"
	case errors.Is(err, access.ErrInvalidScope):
		return http.StatusBadRequest, "invalid scope"
	case errors.Is(err, access.ErrUnavailable):
		return http.StatusServiceUnavailable, "skills authorization unavailable"
	default:
		return http.StatusInternalServerError, "internal error"
	}
}

// beginAgentSkillAccess opens one Skill authorization decision and folds the
// route agent's read gate into it, so both the skill and its path-agent gate
// share a single authorization for every scope (including user/system DB skills).
// It replaces the preliminary requireAgentAccess split decision on the
// agent-scoped skill endpoints. Returns (code, msg) != 0 for the caller to write
// on failure.
func (s *Server) beginAgentSkillAccess(ctx context.Context, agentID string) (*access.Access, int, string) {
	acc, code, msg := s.beginSkillAccess(ctx)
	if code != 0 {
		return nil, code, msg
	}
	if err := acc.AuthorizeAgent(ctx, agentID); err != nil {
		code, msg := skillAccessError(err)
		return nil, code, msg
	}
	return acc, 0, ""
}

// authorizeReadableDBSkills filters DB skill rows through the Skill read PEP under
// the caller's evaluation (the route agent already gated on the same acc): it
// decides the collection once, then drops each row the caller may not read. The
// FS project/built-in merge is applied by the caller afterward, so filesystem
// skills are never gated here. On an unexpected authorization failure it writes
// the response and returns ok=false.
func (s *Server) authorizeReadableDBSkills(w http.ResponseWriter, r *http.Request, acc *access.Access, dbSkills []skill.Skill) ([]skill.Skill, bool) {
	if err := acc.AuthorizeList(); err != nil {
		code, msg := skillAccessError(err)
		writeError(w, code, msg)
		return nil, false
	}
	out := make([]skill.Skill, 0, len(dbSkills))
	for _, sk := range dbSkills {
		err := acc.AuthorizeRead(r.Context(), sk)
		switch {
		case err == nil:
			revision, loadErr := s.skills.LoadCurrentRevision(r.Context(), sk)
			if skill.IsCurrentSelectorMissing(loadErr) {
				s.warnMissingSkillSelector(sk, loadErr)
				continue
			}
			if loadErr != nil {
				s.writeInternalError(w, loadErr)
				return nil, false
			}
			sk = revision.Skill
			out = append(out, sk)
		case errors.Is(err, access.ErrNotFound), errors.Is(err, access.ErrForbidden):
			// filtered
		default:
			code, msg := skillAccessError(err)
			writeError(w, code, msg)
			return nil, false
		}
	}
	return out, true
}

func (s *Server) warnMissingSkillSelector(identity skill.Skill, err error) {
	if s.log != nil {
		s.log.Warn("skip Skill with missing current selector", "skill_id", identity.ID, "scope", identity.Scope, "error", err)
	}
}

// authorizeDBSkillRead authorizes reading one resolved DB-backed skill
// through the Skill read PEP, reusing the acc that already gated the route agent
// so the agent and skill decisions share one evaluation. Immutable project and
// built-in skills pass. On denial it writes the response and returns false.
func (s *Server) authorizeDBSkillRead(w http.ResponseWriter, r *http.Request, acc *access.Access, rs *skill.ResolvedSkill) bool {
	if rs == nil || rs.IsImmutable() {
		return true
	}
	if err := acc.AuthorizeRead(r.Context(), resolvedToDBSkill(rs)); err != nil {
		code, msg := skillAccessError(err)
		writeError(w, code, msg)
		return false
	}
	return true
}

// resolvedToDBSkill projects a resolved (FS-or-DB) skill into the durable row
// facts the Skill PEP authorizes against. Only DB rows reach it.
func resolvedToDBSkill(rs *skill.ResolvedSkill) skill.Skill {
	return skill.Skill{
		ID:            rs.ID,
		Scope:         rs.Scope,
		UserID:        rs.UserID,
		AgentID:       rs.AgentID,
		Name:          rs.Name,
		Metadata:      rs.Metadata,
		ContentDigest: rs.ContentDigest,
	}
}

func (s *Server) skillService() *skill.Service {
	return skill.NewService()
}

// requireAgentAccess authorizes read/use access to an agent through the agent
// PEP. It is the chokepoint every agent-scoped sub-resource handler calls.
func (s *Server) requireAgentAccess(ctx context.Context, agentID string) (config.Agent, int, string) {
	info := UserFromContext(ctx)
	if info == nil {
		return config.Agent{}, http.StatusUnauthorized, "unauthorized"
	}
	authority, err := info.authority()
	if err != nil {
		return config.Agent{}, http.StatusForbidden, "forbidden"
	}
	a, err := s.agentAccess.Read(ctx, authority, agentID)
	if err != nil {
		code, msg := agentAccessError(err)
		if code == http.StatusInternalServerError {
			s.log.Error("agent access", "agent_id", agentID, "error", err)
		}
		return config.Agent{}, code, msg
	}
	return a, 0, ""
}

// requireAgentUse authorizes executing a turn against an agent.
func (s *Server) requireAgentUse(ctx context.Context, agentID string) (config.Agent, int, string) {
	return s.requireAgentAction(ctx, agentID, "use", s.agentAccess.Use)
}

// requireAgentManage authorizes managing (updating) an agent through the agent
// PEP (admin, or the agent's creator via the creator-manage policy).
func (s *Server) requireAgentManage(ctx context.Context, agentID string) (config.Agent, int, string) {
	return s.requireAgentAction(ctx, agentID, "manage", s.agentAccess.Manage)
}

func (s *Server) requireAgentAction(ctx context.Context, agentID, action string, decide func(context.Context, authz.Authority, string) (config.Agent, error)) (config.Agent, int, string) {
	info := UserFromContext(ctx)
	if info == nil {
		return config.Agent{}, http.StatusUnauthorized, "unauthorized"
	}
	authority, err := info.authority()
	if err != nil {
		return config.Agent{}, http.StatusForbidden, "forbidden"
	}
	a, err := decide(ctx, authority, agentID)
	if err != nil {
		code, msg := agentAccessError(err)
		if code == http.StatusInternalServerError {
			s.log.Error("agent "+action, "agent_id", agentID, "error", err)
		}
		return config.Agent{}, code, msg
	}
	return a, 0, ""
}

// ---- helpers ----------------------------------------------------------------

func (s *Server) projectSkillSnapshotForSession(ctx context.Context, agentID string, sessionID *string) (*skill.ProjectSnapshot, error) {
	if sessionID == nil || *sessionID == "" {
		return nil, nil
	}
	info := UserFromContext(ctx)
	if info == nil {
		return nil, errors.New("unauthorized")
	}
	authority, err := info.authority()
	if err != nil {
		return nil, err
	}
	access, err := s.sessionAccess.Begin(ctx, authority)
	if err != nil {
		return nil, err
	}
	session, err := access.Read(ctx, agentID, *sessionID)
	if err != nil {
		return nil, err
	}
	if session.ProjectID == "" {
		return nil, nil
	}
	if s.projectStore == nil {
		return nil, errors.New("project store unavailable")
	}
	snapshot, _, err := s.projectStore.SnapshotSkills(ctx, session.ProjectID, session.UserID, session.AgentID)
	return snapshot, err
}

func resolvedSkillToView(rs skill.ResolvedSkill) skillView {
	var files []string
	if immutableFiles := rs.ImmutableFiles(); immutableFiles != nil {
		files = immutableFiles
	}
	if files == nil {
		files = []string{}
	}
	return skillView{
		ID:                     rs.ID,
		Scope:                  rs.Scope,
		UserID:                 rs.UserID,
		AgentID:                rs.AgentID,
		Name:                   rs.Name,
		Description:            rs.Description,
		DisableModelInvocation: rs.DisableModelInvocation,
		Files:                  files,
		Source:                 skillSource(rs.Metadata),
		Version:                skillVersion(rs.Metadata),
		LifecycleVersion:       rs.Version,
		ContentDigest:          rs.ContentDigest,
		CreatedBy:              skillCreatedBy(rs.Metadata),
		CreatedAt:              rs.CreatedAt.UTC(),
		UpdatedAt:              rs.UpdatedAt.UTC(),
	}
}

// skillSource extracts the install source recorded in a skill's metadata, if any.
func skillSource(metadata json.RawMessage) string {
	return skillMeta(metadata).Source
}

// skillVersion extracts the installed version recorded in a skill's metadata
// (git ref/commit or clawhub version), if any.
func skillVersion(metadata json.RawMessage) string {
	return skillMeta(metadata).Version
}

func skillMeta(metadata json.RawMessage) struct {
	Source  string `json:"source"`
	Version string `json:"version"`
} {
	var m struct {
		Source  string `json:"source"`
		Version string `json:"version"`
	}
	if len(metadata) > 0 {
		_ = json.Unmarshal(metadata, &m)
	}
	return m
}

// agentSkillWriteScope validates a DB-backed write scope reached through an
// agent-scoped path and authorizes it through the Skill PEP, returning the acting
// user id used for owner columns, install attribution, and token lookup. The
// agent path always carries an agent, so user/user_agent/system_agent are the
// only writable scopes here; bare system and project are managed elsewhere. The
// PEP folds the agent-read gate for the path agent into its single evaluation.
func (s *Server) agentSkillWriteScope(ctx context.Context, agentID, scope string) (string, int, string) {
	info := UserFromContext(ctx)
	if info == nil {
		return "", http.StatusUnauthorized, "unauthorized"
	}
	switch scope {
	case "user", "user_agent", "system_agent":
		// authorized through the PEP below
	case "project":
		return "", http.StatusBadRequest, "project skills are managed via the CLI or filesystem"
	case "system":
		return "", http.StatusForbidden, "system skills are managed in Settings → Skills"
	default:
		return "", http.StatusBadRequest, "scope must be one of: user, user_agent, system_agent"
	}
	acc, code, msg := s.beginSkillAccess(ctx)
	if code != 0 {
		return "", code, msg
	}
	if _, _, err := acc.AuthorizeManageScope(ctx, scope, agentID); err != nil {
		code, msg := skillAccessError(err)
		return "", code, msg
	}
	return info.UserID, 0, ""
}

// resolveAgentSkillReference treats a managed Skill ID as authoritative, then
// resolves the API's name-based project and builtin references from one exact
// identity/revision snapshot.
func (s *Server) resolveAgentSkillReference(ctx context.Context, agentID, ref, scope string, exactScope bool, sessionID *string) (*skill.ResolvedSkill, *access.Access, string, int, string) {
	info := UserFromContext(ctx)
	if info == nil {
		return nil, nil, "", http.StatusUnauthorized, "unauthorized"
	}
	acc, code, msg := s.beginAgentSkillAccess(ctx, agentID)
	if code != 0 {
		return nil, nil, "", code, msg
	}
	var identity *skill.Skill
	builtinReference := strings.HasPrefix(ref, "builtin-") || strings.HasPrefix(ref, "builtin:")
	if !builtinReference {
		var err error
		identity, err = s.skills.GetIdentity(ctx, ref)
		if err != nil {
			s.log.Error("find skill by stable id", "agent_id", agentID, "skill", ref, "error", err)
			return nil, nil, "", http.StatusInternalServerError, "internal error"
		}
	}
	if identity != nil {
		applicable := (!exactScope || identity.Scope == scope) && ((identity.Scope != "user_agent" && identity.Scope != "system_agent") || identity.AgentID == agentID)
		if applicable {
			if err := acc.AuthorizeRead(ctx, *identity); err != nil {
				code, msg := skillAccessError(err)
				return nil, nil, "", code, msg
			}
			revision, loadErr := s.skills.LoadCurrentRevision(ctx, *identity)
			if loadErr != nil {
				return nil, nil, "", http.StatusInternalServerError, "internal error"
			}
			sk := revision.Skill
			if sk.Status == "deprecated" {
				return nil, nil, "", http.StatusNotFound, "skill not found"
			}
			return &skill.ResolvedSkill{Skill: sk}, acc, "", 0, ""
		}
		if !exactScope {
			return nil, nil, "", http.StatusNotFound, "skill not found"
		}
		// In an exact-scope request, an ID collision outside the requested
		// scope/agent is not authoritative. Continue with the API's scoped-name
		// resolution so a legal hexadecimal Skill name remains reachable.
	}

	snapshot, snapshotErr := s.projectSkillSnapshotForSession(ctx, agentID, sessionID)
	if snapshotErr != nil {
		return nil, nil, "", http.StatusInternalServerError, "internal error"
	}
	if err := acc.AuthorizeList(); err != nil {
		code, msg := skillAccessError(err)
		return nil, nil, "", code, msg
	}
	identities, err := s.skills.ListIdentityVisible(ctx, skill.ViewContext{UserID: info.UserID, AgentID: agentID})
	if err != nil {
		return nil, nil, "", http.StatusInternalServerError, "internal error"
	}
	dbSkills := make([]skill.Skill, 0, len(identities))
	for _, candidate := range identities {
		if err := acc.AuthorizeRead(ctx, candidate); err != nil {
			if errors.Is(err, access.ErrNotFound) || errors.Is(err, access.ErrForbidden) {
				continue
			}
			return nil, nil, "", http.StatusInternalServerError, "internal error"
		}
		revision, err := s.skills.LoadCurrentRevision(ctx, candidate)
		if skill.IsCurrentSelectorMissing(err) {
			s.warnMissingSkillSelector(candidate, err)
			continue
		}
		if err != nil {
			return nil, nil, "", http.StatusInternalServerError, "internal error"
		}
		dbSkills = append(dbSkills, revision.Skill)
	}
	// Exact mutable-scope management must not be hidden by a higher-priority
	// same-name project Skill. The identity and actor were authorized before
	// the revision was opened above.
	if exactScope && scope != "project" && scope != "builtin" {
		for _, candidate := range dbSkills {
			if candidate.Scope == scope && candidate.Name == ref && candidate.Status != skill.SkillStatusDeprecated {
				return &skill.ResolvedSkill{Skill: candidate}, acc, "", 0, ""
			}
		}
		if scope != "system" {
			return nil, nil, "", http.StatusNotFound, "skill not found"
		}
	}
	merged := s.skillService().ListMerged(dbSkills, snapshot)
	builtinName := strings.TrimPrefix(strings.TrimPrefix(ref, "builtin:"), "builtin-")
	for i := range merged {
		contextualScope := merged[i].Scope
		if merged[i].BuiltinFiles() != nil {
			contextualScope = "builtin"
		}
		matches := merged[i].ID == ref || merged[i].Name == ref || (builtinReference && merged[i].Name == builtinName)
		if matches && (!exactScope || contextualScope == scope) && merged[i].Status != skill.SkillStatusDeprecated {
			return &merged[i], acc, "", 0, ""
		}
	}
	return nil, nil, "", http.StatusNotFound, "skill not found"
}

// loadSkillFile loads a file from an already-resolved skill.
func (s *Server) loadSkillFile(ctx context.Context, rs *skill.ResolvedSkill, path string) (string, error) {
	if rs.IsImmutable() {
		return rs.LoadImmutableFile(path)
	}
	revision, err := s.skills.LoadExactRevision(ctx, resolvedToDBSkill(rs), rs.ContentDigest)
	if err != nil {
		return "", err
	}
	content, ok := revision.Files[path]
	if !ok {
		return "", fs.ErrNotExist
	}
	return string(content), nil
}

// ---- Agent skills: /api/agents/{id}/skills* ---------------------------------

func (s *Server) ListAgentSkills(w http.ResponseWriter, r *http.Request, id string, params apiserver.ListAgentSkillsParams) {
	agentID := id
	info := UserFromContext(r.Context())
	if info == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	// One Skill evaluation gates the route agent and every DB row: the agent-read
	// gate is folded in (no separate requireAgentAccess evaluation).
	acc, code, msg := s.beginAgentSkillAccess(r.Context(), agentID)
	if code != 0 {
		writeError(w, code, msg)
		return
	}
	policy, err := s.agentSkillPolicy.ReadAgentSkillPolicy(r.Context(), agentID)
	if err != nil {
		s.writeManagedSkillError(w, err)
		return
	}
	if params.Scope != nil && params.ScopeGroup != nil {
		writeError(w, http.StatusBadRequest, "scope and scope_group are mutually exclusive")
		return
	}
	if params.Scope != nil && !params.Scope.Valid() {
		writeError(w, http.StatusBadRequest, "invalid scope")
		return
	}
	if params.ScopeGroup != nil && !params.ScopeGroup.Valid() {
		writeError(w, http.StatusBadRequest, "invalid scope_group")
		return
	}
	pageSize := defaultPageSize
	if params.PageSize != nil {
		pageSize = *params.PageSize
	}
	if pageSize < 1 || pageSize > 100 {
		writeError(w, http.StatusBadRequest, "page_size must be between 1 and 100")
		return
	}
	pageQuery := normalizedSkillPageQuery(info.UserID, agentID, params)
	var cursor *skill.ManagedSkillCursor
	if params.PageToken != nil {
		var err error
		cursor, err = decodeSkillPageToken(*params.PageToken, pageQuery)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	query := ""
	if params.Q != nil {
		query = strings.TrimSpace(*params.Q)
	}
	projectSnapshot, err := s.projectSkillSnapshotForSession(r.Context(), agentID, params.SessionId)
	if err != nil {
		s.writeManagedSkillError(w, err)
		return
	}
	dbSkills, err := s.skills.ListIdentityVisible(r.Context(), skill.ViewContext{UserID: info.UserID, AgentID: agentID})
	if err != nil {
		s.writeManagedSkillError(w, err)
		return
	}
	// Every DB row is authorized under the same evaluation before it is merged with
	// the (ungated) filesystem project/built-in skill.
	dbSkills, ok := s.authorizeReadableDBSkills(w, r, acc, dbSkills)
	if !ok {
		return
	}
	merged := s.skillService().ListMerged(dbSkills, projectSnapshot)
	filtered := make([]skill.ResolvedSkill, 0, len(merged))
	queryLower := strings.ToLower(query)
	for _, rs := range merged {
		if rs.Status == skill.SkillStatusDeprecated {
			continue
		}
		if queryLower != "" && !strings.Contains(strings.ToLower(rs.Name), queryLower) && !strings.Contains(strings.ToLower(rs.Description), queryLower) {
			continue
		}
		filtered = append(filtered, rs)
	}

	counts := agentSkillScopeCounts(filtered)
	selected := make([]skill.ResolvedSkill, 0, len(filtered))
	for _, rs := range filtered {
		if params.Scope != nil && string(*params.Scope) == "builtin" {
			if rs.BuiltinFiles() != nil {
				selected = append(selected, rs)
			}
			continue
		}
		if agentSkillScopeSelected(rs.Scope, params) {
			selected = append(selected, rs)
		}
	}
	total := len(selected)
	sort.SliceStable(selected, func(i, j int) bool {
		if selected[i].UpdatedAt.Equal(selected[j].UpdatedAt) {
			return selected[i].ID > selected[j].ID
		}
		return selected[i].UpdatedAt.After(selected[j].UpdatedAt)
	})
	if cursor != nil {
		position := 0
		for position < len(selected) && !skillFollowsCursor(selected[position], *cursor) {
			position++
		}
		selected = selected[position:]
	}

	hasMore := len(selected) > pageSize
	if hasMore {
		selected = selected[:pageSize]
	}
	out := make([]skillView, len(selected))
	for i := range selected {
		out[i] = s.contextualSkillView(selected[i], policy)
	}
	canManage := false
	if authority, err := info.authority(); err == nil {
		_, err = s.agentAccess.Manage(r.Context(), authority, agentID)
		canManage = err == nil
	}
	policyRefs := policyAddressableSkillRefs(dbSkills)
	dangling := make([]string, 0, len(policy.Disabled))
	for _, ref := range policy.Disabled {
		// Legacy builtin refs remain readable in storage but no longer have a
		// policy effect or an independent management surface.
		if strings.HasPrefix(ref, "builtin:") {
			continue
		}
		if !policyRefs[ref] {
			dangling = append(dangling, ref)
		}
	}
	response := map[string]any{
		"skills": out, "total_size": total, "scope_counts": counts, "next_page_token": nil,
		"can_manage_activation": canManage,
		"policy_diagnostics":    map[string]any{"dangling_disabled_refs": dangling},
	}
	if hasMore {
		last := selected[len(selected)-1]
		token, err := encodeSkillPageToken(skill.ManagedSkillCursor{Timestamp: last.UpdatedAt, ID: last.ID}, pageQuery)
		if err != nil {
			s.writeManagedSkillError(w, err)
			return
		}
		response["next_page_token"] = token
	}
	writeData(w, http.StatusOK, response)
}

// policyAddressableSkillRefs builds diagnostics from the full applicable DB
// catalog, before precedence merging hides shadowed rows. Policy refs describe
// addressable catalog entries, not the one current UI winner.
func policyAddressableSkillRefs(dbSkills []skill.Skill) map[string]bool {
	refs := make(map[string]bool, len(dbSkills))
	for _, sk := range dbSkills {
		if sk.Status == skill.SkillStatusDeprecated {
			continue
		}
		if sk.Scope == "system" || sk.Scope == "system_agent" {
			refs[sk.Scope+":"+sk.Name] = true
		}
	}
	return refs
}

func normalizedSkillPageQuery(userID, agentID string, params apiserver.ListAgentSkillsParams) skillPageQuery {
	query := skillPageQuery{UserID: userID, AgentID: agentID}
	if params.Scope != nil {
		query.Scope = string(*params.Scope)
	}
	if params.ScopeGroup != nil {
		query.ScopeGroup = string(*params.ScopeGroup)
	}
	if params.Q != nil {
		query.Query = strings.ToLower(strings.TrimSpace(*params.Q))
	}
	if params.SessionId != nil {
		query.SessionID = *params.SessionId
	}
	return query
}

func agentSkillScopeGroup(scope string) string {
	switch scope {
	case "system":
		return "system"
	case "system_agent", "user_agent":
		return "agent"
	case "user":
		return "user"
	case "project":
		return "project"
	default:
		return ""
	}
}

func agentSkillScopeSelected(scope string, params apiserver.ListAgentSkillsParams) bool {
	if params.Scope != nil {
		if string(*params.Scope) == "builtin" {
			return false // Builtins are identified from the resolved descriptor above.
		}
		return scope == string(*params.Scope)
	}
	if params.ScopeGroup != nil {
		return agentSkillScopeGroup(scope) == string(*params.ScopeGroup)
	}
	return true
}

func agentSkillScopeCounts(items []skill.ResolvedSkill) map[string]int {
	counts := map[string]int{"all": len(items), "builtin": 0, "system": 0, "agent": 0, "user": 0, "project": 0}
	for i := range items {
		if items[i].BuiltinFiles() != nil {
			counts["builtin"]++
			continue
		}
		if group := agentSkillScopeGroup(items[i].Scope); group != "" {
			counts[group]++
		}
	}
	return counts
}

func skillFollowsCursor(sk skill.ResolvedSkill, cursor skill.ManagedSkillCursor) bool {
	return sk.UpdatedAt.Before(cursor.Timestamp) || (sk.UpdatedAt.Equal(cursor.Timestamp) && sk.ID < cursor.ID)
}

func (s *Server) CreateAgentSkill(w http.ResponseWriter, r *http.Request, id string) {
	agentID := id
	var req createSkillRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Scope == "" {
		writeError(w, http.StatusBadRequest, "scope is required")
		return
	}
	userID, code, msg := s.agentSkillWriteScope(r.Context(), agentID, req.Scope)
	if code != 0 {
		writeError(w, code, msg)
		return
	}
	files := req.Files
	if files == nil {
		files = map[string]string{skill.MainFile: "---\nname: " + req.Name + "\ndescription: " + req.Description + "\n---\n"}
	}
	if files[skill.MainFile] == "" {
		writeError(w, http.StatusBadRequest, "files must include SKILL.md")
		return
	}
	sk := skill.Skill{
		Scope:                  req.Scope,
		Name:                   req.Name,
		Description:            req.Description,
		DisableModelInvocation: req.DisableModelInvocation,
	}
	switch req.Scope {
	case "user":
		sk.UserID = userID
	case "user_agent":
		sk.UserID = userID
		sk.AgentID = agentID
	case "system_agent":
		sk.AgentID = agentID
	}
	snapshot, err := s.skills.CreateManagedSkill(r.Context(), sk, files)
	if err != nil {
		if errors.Is(err, skill.ErrInvalidSkillFilePath) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.writeManagedSkillError(w, err)
		return
	}
	writeData(w, http.StatusCreated, committedSkillView(snapshot))
}

func (s *Server) GetAgentSkill(w http.ResponseWriter, r *http.Request, id string, skillId string, params apiserver.GetAgentSkillParams) {
	scope := ""
	exactScope := params.Scope != nil
	if params.Scope != nil {
		scope = string(*params.Scope)
	}
	rs, acc, _, code, msg := s.resolveAgentSkillReference(r.Context(), id, skillId, scope, exactScope, params.SessionId)
	if code != 0 {
		writeError(w, code, msg)
		return
	}
	if !s.authorizeDBSkillRead(w, r, acc, rs) {
		return
	}
	policy, err := s.agentSkillPolicy.ReadAgentSkillPolicy(r.Context(), id)
	if err != nil {
		s.writeManagedSkillError(w, err)
		return
	}
	view := s.contextualSkillView(*rs, policy)
	if !rs.IsImmutable() {
		sk := resolvedToDBSkill(rs)
		managedView, err := s.dbSkillView(r, &sk)
		if err != nil {
			s.writeManagedSkillError(w, err)
			return
		}
		view = managedView
		enabled := true
		if ref, ok := skill.PolicyRef(*rs); ok {
			view.LogicalRef = ref
			enabled = !policy.DisabledRef(ref)
		}
		view.Enabled = &enabled
	}
	writeData(w, http.StatusOK, view)
}

func (s *Server) UpdateAgentSkill(w http.ResponseWriter, r *http.Request, id string, skillId string, params apiserver.UpdateAgentSkillParams) {
	rs, acc, _, code, msg := s.resolveAgentSkillReference(r.Context(), id, skillId, string(params.Scope), true, params.SessionId)
	if code != 0 {
		writeError(w, code, msg)
		return
	}

	if rs.IsImmutable() {
		if rs.Scope == "system" {
			writeError(w, http.StatusForbidden, "system skills are read-only")
			return
		}
		writeError(w, http.StatusBadRequest, "project skills are read-only; edit them in the project workspace")
		return
	}

	// Load and authorize the durable row by stable ID before applying lifecycle-aware updates.
	sk, err := acc.AuthorizeManageByID(r.Context(), rs.ID, authz.ActionWrite)
	if err != nil {
		code, msg := skillAccessError(err)
		writeError(w, code, msg)
		return
	}
	s.applySkillUpdate(w, r, &sk)
}

// UpgradeAgentSkill re-fetches a DB-backed skill from its recorded install source
// and updates it in place when the source has a newer version. It is the
// check-and-update behind the inspector's "check for updates" button.
func (s *Server) UpgradeAgentSkill(w http.ResponseWriter, r *http.Request, id string, skillId string, params apiserver.UpgradeAgentSkillParams) {
	scope := ""
	exactScope := params.Scope != nil
	if params.Scope != nil {
		scope = *params.Scope
	}
	rs, acc, _, code, msg := s.resolveAgentSkillReference(r.Context(), id, skillId, scope, exactScope, nil)
	if code != 0 {
		writeError(w, code, msg)
		return
	}
	if rs.IsImmutable() {
		writeError(w, http.StatusBadRequest, "only installed skills can be upgraded")
		return
	}

	if _, err := acc.AuthorizeManageByID(r.Context(), rs.ID, authz.ActionWrite); err != nil {
		code, msg := skillAccessError(err)
		writeError(w, code, msg)
		return
	}
	actingUserID := ""
	if info := UserFromContext(r.Context()); info != nil {
		actingUserID = info.UserID
	}

	ctx := r.Context()
	if skill.GitHubSource(skillSource(rs.Metadata)) {
		if token := s.credSvc.GitHubAccessToken(ctx, actingUserID); token != "" {
			ctx = skill.WithGitHubToken(ctx, token)
		}
	}

	res, err := skill.UpgradeInStore(ctx, s.skills, resolvedToDBSkill(rs), params.ExpectedDigest, rs.Metadata)
	if err != nil {
		if errors.Is(err, skill.ErrNoUpgradeSource) {
			writeError(w, http.StatusBadRequest, "skill was not installed from an upgradable source")
			return
		}
		if errors.Is(err, skill.ErrSkillDigestRequired) || errors.Is(err, skill.ErrSkillDigestConflict) {
			s.writeSkillMutationError(w, err)
			return
		}
		s.writeBadGatewayError(w, err)
		return
	}
	writeData(w, http.StatusOK, map[string]any{
		"updated":          res.Updated,
		"version":          res.Version,
		"previous_version": res.PreviousVersion,
	})
}

func (s *Server) DeleteAgentSkill(w http.ResponseWriter, r *http.Request, id string, skillId string, params apiserver.DeleteAgentSkillParams) {
	rs, acc, _, code, msg := s.resolveAgentSkillReference(r.Context(), id, skillId, string(params.Scope), true, params.SessionId)
	if code != 0 {
		writeError(w, code, msg)
		return
	}

	if rs.IsImmutable() {
		if rs.Scope == "system" {
			writeError(w, http.StatusForbidden, "system skills are read-only")
			return
		}
		writeError(w, http.StatusBadRequest, "project skills are read-only; delete them in the project workspace")
		return
	}

	if _, err := acc.AuthorizeManageByID(r.Context(), rs.ID, authz.ActionDelete); err != nil {
		code, msg := skillAccessError(err)
		writeError(w, code, msg)
		return
	}
	expectedDigest := ""
	if params.ExpectedDigest != nil {
		expectedDigest = *params.ExpectedDigest
	}
	s.doDeleteSkill(w, r, resolvedToDBSkill(rs), expectedDigest)
}

func (s *Server) GetAgentSkillFile(w http.ResponseWriter, r *http.Request, id string, skillId string, params apiserver.GetAgentSkillFileParams) {
	scope := ""
	exactScope := params.Scope != nil
	if params.Scope != nil {
		scope = string(*params.Scope)
	}
	rs, acc, _, code, msg := s.resolveAgentSkillReference(r.Context(), id, skillId, scope, exactScope, params.SessionId)
	if code != 0 {
		writeError(w, code, msg)
		return
	}
	if !s.authorizeDBSkillRead(w, r, acc, rs) {
		return
	}
	content, err := s.loadSkillFile(r.Context(), rs, params.Path)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeData(w, http.StatusOK, skillFileResponse(params.Path, content))
}

func (s *Server) DeleteAgentSkillFile(w http.ResponseWriter, r *http.Request, id string, skillId string, params apiserver.DeleteAgentSkillFileParams) {
	rs, acc, _, code, msg := s.resolveAgentSkillReference(r.Context(), id, skillId, string(params.Scope), true, params.SessionId)
	if code != 0 {
		writeError(w, code, msg)
		return
	}

	if rs.IsImmutable() {
		if rs.Scope == "system" {
			writeError(w, http.StatusForbidden, "system skills are read-only")
			return
		}
		writeError(w, http.StatusBadRequest, "project skills are read-only; edit them in the project workspace")
		return
	}

	if err := acc.AuthorizeManage(r.Context(), resolvedToDBSkill(rs), authz.ActionWrite); err != nil {
		code, msg := skillAccessError(err)
		writeError(w, code, msg)
		return
	}
	expectedDigest := ""
	if params.ExpectedDigest != nil {
		expectedDigest = *params.ExpectedDigest
	}
	s.doDeleteSkillFile(w, r, resolvedToDBSkill(rs), params.Path, expectedDigest)
}

func (s *Server) InstallAgentSkill(w http.ResponseWriter, r *http.Request, id string) {
	agentID := id
	var req installSkillRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Source == "" {
		writeError(w, http.StatusBadRequest, "source is required")
		return
	}
	scope := req.Scope
	if scope == "" {
		writeError(w, http.StatusBadRequest, "scope is required")
		return
	}
	userID, code, msg := s.agentSkillWriteScope(r.Context(), agentID, scope)
	if code != 0 {
		writeError(w, code, msg)
		return
	}
	storeUserID := ""
	if scope == "user" || scope == "user_agent" {
		storeUserID = userID
	}
	ctx := r.Context()
	if skill.GitHubSource(req.Source) {
		if token := s.credSvc.GitHubAccessToken(ctx, userID); token != "" {
			ctx = skill.WithGitHubToken(ctx, token)
		}
	}
	snapshot, err := skill.InstallToStore(ctx, s.skills, req.Source, scope, storeUserID, agentID)
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "a skill with this name is already installed in this scope")
			return
		}
		s.writeManagedSkillError(w, err)
		return
	}
	writeData(w, http.StatusCreated, committedSkillView(snapshot))
}

func (s *Server) UploadAgentSkill(w http.ResponseWriter, r *http.Request, id string) {
	s.uploadAgentSkill(w, r, id) //nolint:contextcheck
}
