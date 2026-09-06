package plugin

import (
	"context"
	"sync/atomic"

	"github.com/CherryHQ/stella/internal/authz"
)

var ErrForbidden = authz.ErrForbidden

// Access is the authority-bound entrypoint for all plugin CRUD. Agent ownership
// is always checked through the central Agent PEP before a tuple is returned.
type Access struct {
	service   *Service
	authority authz.Authority
	active    *atomic.Bool
}

func (a *Access) ensureActive() error {
	if a == nil || a.service == nil || !a.authority.Valid() {
		return ErrForbidden
	}
	if a.active != nil && !a.active.Load() {
		return ErrForbidden
	}
	return nil
}

func (a *Access) owner(ctx context.Context, scope Scope, agentID string) (string, string, error) {
	if err := a.ensureActive(); err != nil || a.authority.Kind() != authz.ActorUser {
		return "", "", ErrForbidden
	}
	userID := string(a.authority.UserID())
	switch scope {
	case ScopeSystem:
		if !a.authority.IsAdmin() || agentID != "" {
			return "", "", ErrForbidden
		}
		return "", "", nil
	case ScopeSystemAgent:
		if !a.authority.IsAdmin() || agentID == "" || a.service.agents == nil {
			return "", "", ErrForbidden
		}
		if err := a.service.agents.Authorize(ctx, a.authority, agentID, authz.ActionRead); err != nil {
			return "", "", err
		}
		return "", agentID, nil
	case ScopeUser:
		if agentID != "" {
			return "", "", ErrForbidden
		}
		return userID, "", nil
	case ScopeUserAgent:
		if agentID == "" || a.service.agents == nil {
			return "", "", ErrForbidden
		}
		if err := a.service.agents.Authorize(ctx, a.authority, agentID, authz.ActionRead); err != nil {
			return "", "", err
		}
		return userID, agentID, nil
	default:
		return "", "", ErrUnknownScope
	}
}
