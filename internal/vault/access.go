package vault

import (
	"context"
	"errors"
	"fmt"

	"github.com/CherryHQ/stella/internal/authz"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
)

// Access captures one validated authority for a vault use case. Vault owns the
// direct rules for ResourceVault: the HTTP handlers and the agent tool pass a
// trusted authz.Authority and never a bare identity or an IsAdmin bool. The four
// durable scopes decide this way:
//   - user / user_agent are user-owned (is_owner is derived from the entry's
//     owner column; an agent-scoped actor is confined to its own user_agent
//     bucket, and every agent-scoped op folds an agent-read gate);
//   - system / system_agent are admin-managed and reachable only by an admin.
//
// Trusted internal callers (MCP, connections/OAuth, email, channel config,
// sandbox env loader, key provisioning) keep using the raw Service methods; they
// are host-side credential plumbing, not user requests, and never open an Access.
type Access struct {
	svc         *Service
	authority   authz.Authority
	userID      string
	agentID     string
	agentScoped bool
}

// Begin captures validated authority for one vault use case.
func (s *Service) Begin(_ context.Context, authority authz.Authority) (*Access, error) {
	if s == nil || s.agents == nil {
		return nil, fmt.Errorf("vault authorization unavailable: agent access not configured")
	}
	if !authority.Valid() {
		return nil, authz.ErrForbidden
	}
	agentID := ""
	agentScoped := false
	if authority.Kind() == authz.ActorAgent {
		agentID = string(authority.AgentID())
		agentScoped = true
	}
	return &Access{svc: s, authority: authority, userID: string(authority.UserID()), agentID: agentID, agentScoped: agentScoped}, nil
}

// ListScoped lists entry metadata for one scope, or (scope == "") the caller's
// user and user_agent buckets aggregated, matching the pre-cutover tool default.
func (a *Access) ListScoped(ctx context.Context, scope, agentID string) ([]EntryMeta, error) {
	if scope == "" {
		userEntries, err := a.listOne(ctx, ScopeUser, "")
		if err != nil {
			return nil, err
		}
		// Only an agent-scoped caller has an implicit user_agent bucket to aggregate.
		if a.agentID == "" {
			return userEntries, nil
		}
		agentEntries, err := a.listOne(ctx, ScopeUserAgent, a.agentID)
		if err != nil {
			return nil, err
		}
		return append(userEntries, agentEntries...), nil
	}
	return a.listOne(ctx, scope, agentID)
}

func (a *Access) listOne(ctx context.Context, scope, agentID string) ([]EntryMeta, error) {
	userID, resolvedAgent, err := a.authorizeScoped(ctx, authz.ActionList, scope, agentID)
	if err != nil {
		return nil, err
	}
	if isSystemScope(scope) {
		return a.hideManagedNames(a.svc.ListSystemScoped(ctx, scope, resolvedAgent))
	}
	return a.hideManagedNames(a.svc.listScoped(ctx, scope, userID, resolvedAgent))
}

// GetScoped decrypts and returns one entry's plaintext value.
func (a *Access) GetScoped(ctx context.Context, scope, agentID, name string) (string, error) {
	userID, resolvedAgent, err := a.authorizeScoped(ctx, authz.ActionRead, scope, agentID)
	if err != nil {
		return "", err
	}
	if err := a.rejectManagedName(name); err != nil {
		return "", err
	}
	return a.svc.GetScoped(ctx, scope, userID, resolvedAgent, name)
}

// GetScopedMeta returns non-sensitive metadata for one entry.
func (a *Access) GetScopedMeta(ctx context.Context, scope, agentID, name string) (EntryMeta, error) {
	userID, resolvedAgent, err := a.authorizeScoped(ctx, authz.ActionRead, scope, agentID)
	if err != nil {
		return EntryMeta{}, err
	}
	if err := a.rejectManagedName(name); err != nil {
		return EntryMeta{}, err
	}
	return a.svc.GetScopedMeta(ctx, scope, userID, resolvedAgent, name)
}

// SetScoped stores an entry after a write decision.
func (a *Access) SetScoped(ctx context.Context, scope, agentID, name, value string, opts SetOptions) error {
	userID, resolvedAgent, err := a.authorizeScoped(ctx, authz.ActionWrite, scope, agentID)
	if err != nil {
		return err
	}
	if err := a.rejectManagedName(name); err != nil {
		return err
	}
	if isSystemScope(scope) {
		return a.svc.SetSystemScopedWithOptions(ctx, scope, resolvedAgent, name, value, opts)
	}
	return a.svc.SetScopedWithOptions(ctx, scope, userID, resolvedAgent, name, value, opts)
}

// DeleteScoped removes an entry after a delete decision.
func (a *Access) DeleteScoped(ctx context.Context, scope, agentID, name string) error {
	userID, resolvedAgent, err := a.authorizeScoped(ctx, authz.ActionDelete, scope, agentID)
	if err != nil {
		return err
	}
	if err := a.rejectManagedName(name); err != nil {
		return err
	}
	if isSystemScope(scope) {
		return a.svc.DeleteSystemScoped(ctx, scope, resolvedAgent, name)
	}
	return a.svc.DeleteScoped(ctx, scope, userID, resolvedAgent, name)
}

func (a *Access) rejectManagedName(name string) error {
	if a.svc.isSystemManagedName(name) {
		return fmt.Errorf("vault: name %q is reserved for system-managed credentials: %w", name, authz.ErrForbidden)
	}
	return nil
}

func (a *Access) hideManagedNames(entries []EntryMeta, err error) ([]EntryMeta, error) {
	if err != nil {
		return nil, err
	}
	visible := entries[:0]
	for _, entry := range entries {
		if !a.svc.isSystemManagedName(entry.Name) {
			visible = append(visible, entry)
		}
	}
	return visible, nil
}

// authorizeScoped resolves an entry's owner/agent columns for a scope, decides the
// action against ResourceVault, and folds the agent-read gate for agent-scoped
// buckets — all under this Access's single revision. It returns the resolved
// (userID, agentID) columns for the durable call. A policy denial is 403
// (ErrForbidden); a system scope is reachable only via admin-full-access.
func (a *Access) authorizeScoped(ctx context.Context, action authz.Action, scope, requestedAgentID string) (string, string, error) {
	var entryUserID, entryAgentID string
	switch scope {
	case ScopeUser:
		if a.userID == "" {
			return "", "", authz.ErrUnauthenticated
		}
		entryUserID = a.userID
	case ScopeUserAgent:
		if a.userID == "" {
			return "", "", authz.ErrUnauthenticated
		}
		// An agent-scoped actor defaults to (and is confined to) its own bucket.
		if requestedAgentID == "" && a.agentScoped {
			requestedAgentID = a.agentID
		}
		if a.agentScoped && requestedAgentID != a.agentID {
			return "", "", authz.ErrForbidden
		}
		entryUserID = a.userID
		entryAgentID = requestedAgentID
	case ScopeSystem:
		// admin-only, decided by policy below.
	case ScopeSystemAgent:
		// admin-only; the agent column is validated after the policy decision so a
		// non-admin is denied (access-denied), not nagged for a missing agent_id.
		entryAgentID = requestedAgentID
	default:
		return "", "", fmt.Errorf("vault: invalid scope %q", scope)
	}
	// Decide the rule FIRST so a caller with no access to a scope is denied before
	// any structural validation of that scope's columns.
	isOwner := !isSystemScope(scope) && a.userID != "" && entryUserID == a.userID
	if !a.allow(action, scope, isOwner) {
		return "", "", authz.ErrForbidden
	}
	// The authorized caller must still name a valid bucket.
	if (scope == ScopeUserAgent || scope == ScopeSystemAgent) && entryAgentID == "" {
		return "", "", fmt.Errorf("vault: agent_id is required for %s scope", scope)
	}
	if err := validateScope(scope, entryUserID, entryAgentID); err != nil {
		return "", "", err
	}
	// Every agent-scoped bucket additionally requires Agent-domain read access.
	if entryAgentID != "" {
		if err := a.authorizeAgent(ctx, entryAgentID); err != nil {
			return "", "", err
		}
	}
	return entryUserID, entryAgentID, nil
}

func (a *Access) authorizeAgent(ctx context.Context, agentID string) error {
	if a.svc.agents == nil {
		return authz.ErrForbidden
	}
	err := a.svc.agents.Authorize(ctx, a.authority, agentID, authz.ActionRead)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, agentaccess.ErrNotFound):
		return authz.ErrNotFound
	case errors.Is(err, agentaccess.ErrForbidden):
		return authz.ErrForbidden
	default:
		return err
	}
}

// allow is Vault's static rule table. Admin holds every scope; an ordinary user
// or a delegated agent may list, and read/create/write/delete only its own
// user/user_agent entries (isOwner). No other actor and no non-admin system-scope
// access exists.
func (a *Access) allow(action authz.Action, scope string, isOwner bool) bool {
	if !action.Valid() {
		return false
	}
	if a.authority.IsAdmin() {
		return true
	}
	switch a.authority.Kind() {
	case authz.ActorUser, authz.ActorAgent:
	default:
		return false
	}
	if isSystemScope(scope) {
		return false
	}
	switch action {
	case authz.ActionList:
		return true
	case authz.ActionRead, authz.ActionCreate, authz.ActionWrite, authz.ActionDelete:
		return isOwner
	default:
		return false
	}
}
