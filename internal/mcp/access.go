package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/CherryHQ/stella/internal/agent"
	"github.com/CherryHQ/stella/internal/authz"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	"github.com/CherryHQ/stella/internal/plugin"
)

// Access binds registration ownership to a verified user authority. Reads use
// the common plugin catalog; mutation methods are being moved to the same
// transaction boundary below.
type Access struct {
	svc       *Service
	agents    *agentaccess.Service
	pools     *agent.PoolManager
	authority authz.Authority
}

func NewAccess(svc *Service, agents *agentaccess.Service, pools *agent.PoolManager) *Access {
	return &Access{svc: svc, agents: agents, pools: pools}
}

func (s *Access) Begin(authority authz.Authority) (*Access, error) {
	if s == nil || s.svc == nil || s.svc.plugins == nil || !authority.Valid() || authority.Kind() != authz.ActorUser {
		return nil, authz.ErrForbidden
	}
	return &Access{svc: s.svc, agents: s.agents, pools: s.pools, authority: authority}, nil
}

func (a *Access) owner(ctx context.Context, scope, agentID string) (string, string, error) {
	if a == nil || a.authority.Kind() != authz.ActorUser {
		return "", "", authz.ErrForbidden
	}
	userID := string(a.authority.UserID())
	switch scope {
	case ScopeUser:
		if agentID != "" {
			return "", "", errors.New("mcp: user scope cannot include agent_id")
		}
		return userID, "", nil
	case ScopeUserAgent:
		if agentID == "" {
			return "", "", errors.New("mcp: user_agent scope requires agent_id")
		}
		if a.agents == nil {
			return "", "", authz.ErrForbidden
		}
		if err := a.agents.Authorize(ctx, a.authority, agentID, authz.ActionRead); err != nil {
			return "", "", err
		}
		return userID, agentID, nil
	case ScopeSystem:
		if !a.authority.IsAdmin() || agentID != "" {
			return "", "", authz.ErrForbidden
		}
		return "", "", nil
	case ScopeSystemAgent:
		if !a.authority.IsAdmin() || agentID == "" || a.agents == nil {
			return "", "", authz.ErrForbidden
		}
		if err := a.agents.Authorize(ctx, a.authority, agentID, authz.ActionRead); err != nil {
			return "", "", err
		}
		return "", agentID, nil
	default:
		return "", "", errors.New("mcp: invalid scope")
	}
}

func (a *Access) List(ctx context.Context, scope, agentID string) ([]Registration, error) {
	if _, _, err := a.owner(ctx, scope, agentID); err != nil {
		return nil, err
	}
	return a.svc.commonRegistrationsByScope(ctx, a.authority, scope, agentID)
}

func (a *Access) Get(ctx context.Context, id, scope, agentID string) (Registration, error) {
	uid, aid, err := a.owner(ctx, scope, agentID)
	if err != nil {
		return Registration{}, err
	}
	reg, err := a.svc.commonRegistration(ctx, a.authority, id)
	if err != nil {
		return Registration{}, err
	}
	if reg.Scope != scope || reg.UserID != uid || reg.AgentID != aid {
		return Registration{}, authz.ErrNotFound
	}
	return reg, nil
}

// Probe is the PEP for the probe entry point: the caller must be able to read
// the registration, and the probe result goes to the row it read. For a
// per_user registration the probe uses the caller's own credential.
func (a *Access) Probe(ctx context.Context, id, scope, agentID string) (Registration, error) {
	uid, aid, err := a.owner(ctx, scope, agentID)
	if err != nil {
		return Registration{}, err
	}
	reg, err := a.svc.commonRegistration(ctx, a.authority, id)
	if err != nil {
		return Registration{}, err
	}
	if reg.Scope != scope || reg.UserID != uid || reg.AgentID != aid {
		return Registration{}, authz.ErrNotFound
	}
	// Per-user credentials always belong to the authenticated user, even when
	// the authored config itself lives in a system scope. Shared credentials
	// resolve from the registration tuple inside CredentialOwner.
	return a.svc.Probe(ctx, reg, a.svc.CredentialOwner(reg, string(a.authority.UserID())))
}

// GetVisible resolves a registration the caller can *see* in the agent
// context, without requiring read authority on the row: system and
// system_agent registrations are visible to every authenticated user (they
// resolve for the user's agents), while user/user_agent rows still demand
// ownership. It exists so a per_user Connect can start from a registration the
// caller may not manage; every write path still goes through owner().
func (a *Access) GetVisible(ctx context.Context, id string) (Registration, error) {
	if a == nil || a.svc == nil || a.svc.plugins == nil || a.svc.pool == nil || a.authority.Kind() != authz.ActorUser {
		return Registration{}, authz.ErrForbidden
	}
	pluginAccess, err := a.svc.plugins.Begin(a.authority)
	if err != nil {
		return Registration{}, err
	}
	var cfg plugin.Config
	var enabled pgtype.Bool
	var userID, agentID pgtype.Text
	var payload, refs []byte
	var scope string
	var createdAt, updatedAt time.Time
	var pluginID string
	err = a.svc.pool.QueryRow(ctx, `
		SELECT id, plugin_id, namespace, scope, user_id, agent_id, enabled, config,
		       credential_refs, revision, created_at, updated_at
		FROM plugin_config WHERE id = $1::uuid`, id).Scan(
		&cfg.ID, &pluginID, &cfg.Namespace, &scope, &userID, &agentID, &enabled,
		&payload, &refs, &cfg.Revision, &createdAt, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Registration{}, authz.ErrNotFound
	}
	if err != nil {
		return Registration{}, fmt.Errorf("mcp: get common registration: %w", err)
	}
	cfg.PluginID, cfg.Scope, cfg.Payload, cfg.CredentialRefs = pluginID, plugin.Scope(scope), payload, refs
	if userID.Valid {
		cfg.UserID = userID.String
	}
	if agentID.Valid {
		cfg.AgentID = agentID.String
	}
	if enabled.Valid {
		cfg.Enabled = &enabled.Bool
	}
	cfg.CreatedAt, cfg.UpdatedAt = createdAt.UTC(), updatedAt.UTC()
	if (cfg.Scope == plugin.ScopeUser || cfg.Scope == plugin.ScopeUserAgent) && cfg.UserID != string(a.authority.UserID()) {
		return Registration{}, authz.ErrForbidden
	}
	if cfg.Scope == plugin.ScopeUserAgent || cfg.Scope == plugin.ScopeSystemAgent {
		if a.agents == nil {
			return Registration{}, authz.ErrForbidden
		}
		if err := a.agents.Authorize(ctx, a.authority, cfg.AgentID, authz.ActionRead); err != nil {
			return Registration{}, err
		}
	}
	if len(cfg.Payload) == 0 {
		return Registration{}, authz.ErrNotFound
	}
	def, err := pluginAccess.GetDefinition(ctx, cfg.PluginID)
	if err != nil {
		return Registration{}, err
	}
	reg, err := a.svc.registrationFromCommonConfig(ctx, a.authority, def, cfg)
	if err != nil {
		return Registration{}, err
	}
	return reg, nil
}

// StartOAuth is the PEP for oauth-start. shared: the caller needs owner
// authority (admin for system scopes). per_user: visibility is enough — the
// caller connects their own account, which never touches shared credentials.
func (a *Access) StartOAuth(ctx context.Context, id, callbackURL string) (Registration, string, string, time.Time, error) {
	reg, err := a.GetVisible(ctx, id)
	if err != nil {
		return Registration{}, "", "", time.Time{}, err
	}
	if reg.CredentialMode != CredentialModePerUser {
		if _, _, err := a.owner(ctx, reg.Scope, reg.AgentID); err != nil {
			return Registration{}, "", "", time.Time{}, err
		}
	}
	authURL, flowID, expiresAt, err := a.svc.StartOAuthForAuthority(ctx, reg, a.authority, callbackURL)
	if err != nil {
		return Registration{}, "", "", time.Time{}, err
	}
	return reg, authURL, flowID, expiresAt, nil
}

// Disconnect is the PEP for oauth-disconnect: shared requires owner authority
// and removes the registration's bundle; per_user removes only the caller's
// own bundle.
func (a *Access) Disconnect(ctx context.Context, id, scope, agentID string) (Registration, error) {
	reg, err := a.GetVisible(ctx, id)
	if err != nil {
		return Registration{}, err
	}
	if reg.CredentialMode != CredentialModePerUser {
		if _, _, err := a.owner(ctx, reg.Scope, reg.AgentID); err != nil {
			return Registration{}, err
		}
	}
	userID := string(a.authority.UserID())
	return a.svc.Disconnect(ctx, reg, userID)
}

// CanRead reports whether the bound authority could read this registration
// through Get. It is the same PEP path, just asked as a question: a user can
// see another user's same-named row in the resolved context but cannot manage
// it, and a non-admin sees system rows it cannot read.
func (a *Access) CanRead(ctx context.Context, reg Registration) bool {
	_, _, err := a.owner(ctx, reg.Scope, reg.AgentID)
	if err != nil {
		return false
	}
	_, err = a.svc.commonRegistration(ctx, a.authority, reg.ID)
	return err == nil
}

func (a *Access) Create(ctx context.Context, in CreateInput) (Registration, error) {
	uid, aid, err := a.owner(ctx, in.Scope, in.AgentID)
	if err != nil {
		return Registration{}, err
	}
	in.UserID, in.AgentID = uid, aid
	in.PluginID = strings.TrimSpace(in.PluginID)
	if in.PluginID == "" {
		// The legacy HTTP create path still reaches this adapter. Keep its raw
		// credential fields fail-closed until the reviewed backend write seam is
		// wired explicitly; Service.CreateCustom is the typed seam for that work.
		if in.Token != "" || in.OAuthClientID != "" || in.OAuthClientSecret != "" {
			return Registration{}, errPluginCredentialsUnavailable
		}
		// The generated settings create action predates common plugin IDs. Its
		// name is already the machine namespace, so validate it verbatim and
		// reject invalid names instead of silently inventing a slug.
		if err := plugin.ValidateNamespace(in.Name); err != nil {
			return Registration{}, err
		}
		def, config, err := a.svc.CreateCustom(authz.WithAuthority(ctx, a.authority), plugin.Definition{
			Namespace: in.Name, DisplayName: in.Name, Backend: plugin.BackendMCP,
			Spec: []byte(`{}`),
		}, in)
		if err != nil {
			return Registration{}, err
		}
		return a.svc.registrationFromCommonConfig(ctx, a.authority, def, config)
	}
	return a.svc.createCommonForAuthority(authz.WithAuthority(ctx, a.authority), a.authority, in)
}

func (a *Access) Update(ctx context.Context, in UpdateInput) (Registration, error) {
	uid, aid, err := a.owner(ctx, in.Scope, in.AgentID)
	if err != nil {
		return Registration{}, err
	}
	in.UserID, in.AgentID = uid, aid
	reg, err := a.svc.updateCommon(ctx, a.authority, in, in.ExpectedVersion)
	if err != nil {
		return Registration{}, err
	}
	a.invalidate(in.Scope, uid, aid)
	a.invalidate(reg.Scope, reg.UserID, reg.AgentID)
	return reg, nil
}

// UpdateIfVersion keeps a Settings mutation's observed version in the service
// transaction, rather than comparing in the tool adapter and writing later.
func (a *Access) UpdateIfVersion(ctx context.Context, in UpdateInput, expectedVersion string) (Registration, error) {
	if strings.TrimSpace(expectedVersion) == "" {
		return Registration{}, ErrVersionConflict
	}
	uid, aid, err := a.owner(ctx, in.Scope, in.AgentID)
	if err != nil {
		return Registration{}, err
	}
	in.UserID, in.AgentID = uid, aid
	reg, err := a.svc.updateCommon(ctx, a.authority, in, expectedVersion)
	if err != nil {
		return Registration{}, err
	}
	a.invalidate(in.Scope, uid, aid)
	a.invalidate(reg.Scope, reg.UserID, reg.AgentID)
	return reg, nil
}

func (a *Access) Delete(ctx context.Context, id, scope, agentID string) error {
	uid, aid, err := a.owner(ctx, scope, agentID)
	if err != nil {
		return err
	}
	reg, err := a.svc.commonRegistration(ctx, a.authority, id)
	if err != nil {
		return err
	}
	if reg.Scope != scope || reg.UserID != uid || reg.AgentID != aid {
		return authz.ErrNotFound
	}
	return a.svc.DeleteCommonConfig(ctx, a.authority, reg.PluginID, reg.ID, reg.ConfigRevision, a.svc.CredentialOwner(reg, string(a.authority.UserID())))
}

// DeleteIfVersion is the Settings delete path with a durable version predicate.
func (a *Access) DeleteIfVersion(ctx context.Context, id, scope, agentID, expectedVersion string) error {
	if strings.TrimSpace(expectedVersion) == "" {
		return ErrVersionConflict
	}
	uid, aid, err := a.owner(ctx, scope, agentID)
	if err != nil {
		return err
	}
	reg, err := a.svc.commonRegistration(ctx, a.authority, id)
	if err != nil {
		return err
	}
	if reg.Scope != scope || reg.UserID != uid || reg.AgentID != aid {
		return authz.ErrNotFound
	}
	if reg.Version() != expectedVersion {
		return ErrVersionConflict
	}
	return a.svc.DeleteCommonConfig(ctx, a.authority, reg.PluginID, reg.ID, reg.ConfigRevision, a.svc.CredentialOwner(reg, string(a.authority.UserID())))
}

func (a *Access) invalidate(scope, userID, agentID string) {
	if a.pools == nil {
		return
	}
	var err error
	switch scope {
	case ScopeUser:
		err = a.pools.InvalidateUser(userID)
	case ScopeUserAgent:
		err = a.pools.InvalidateUserAgent(userID, agentID)
	case ScopeSystemAgent:
		err = a.pools.InvalidateAgent(agentID)
	case ScopeSystem:
		err = a.pools.InvalidateAll()
	}
	if err != nil { // Invalidation is a cache refresh; committed DB state remains authoritative.
		return
	}
}

// Version is intentionally a stable opaque digest of redacted metadata. The
// credential reference and bearer never leave this package, yet any credential
// configuration change still invalidates a stale management write.
func (r Registration) Version() string {
	return fmt.Sprintf("%x", registrationHash(r))
}
