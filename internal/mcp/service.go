package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/plugin"
	vaultpkg "github.com/CherryHQ/stella/internal/vault"

	appdb "github.com/CherryHQ/stella/internal/db"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
)

// McpOauthFlow aliases the generated flow row so the OAuth files do not all
// import sqlc.
type (
	McpOauthFlow             = sqlc.McpOauthFlow
	CreateMCPOAuthFlowParams = sqlc.CreateMCPOAuthFlowParams
)

// DB is the persistence surface the Service needs. *sqlc.Queries satisfies it.
type DB interface {
	CreateMCPOAuthFlow(ctx context.Context, arg CreateMCPOAuthFlowParams) (McpOauthFlow, error)
	ConsumeMCPOAuthFlow(ctx context.Context, id string) (McpOauthFlow, error)
	RenameToolOverridePrefix(ctx context.Context, arg sqlc.RenameToolOverridePrefixParams) error
	DeleteToolOverridesByPrefix(ctx context.Context, prefix string) (int64, error)
}

// Vault stores and retrieves the per-connection bearer token, age-encrypted at
// rest under the same 4-value scope as the registration. *vault.Service
// satisfies it.
type Vault interface {
	SetScoped(ctx context.Context, scope, userID, agentID, name, plaintext string) error
	SetSystemScoped(ctx context.Context, scope, agentID, name, plaintext string) error
	GetScoped(ctx context.Context, scope, userID, agentID, name string) (string, error)
	DeleteScoped(ctx context.Context, scope, userID, agentID, name string) error
	DeleteSystemScoped(ctx context.Context, scope, agentID, name string) error
}

// Service manages MCP server registrations and their encrypted credentials.
// The registration table holds no secret material — the bearer token lives in
// the vault and is referenced by CredentialRef.
type Service struct {
	db        DB
	vault     Vault
	pool      *pgxpool.Pool
	bindVault func(pgx.Tx) Vault
	// plugins is the common configuration owner. MCP keeps this dependency
	// narrow: credential and metadata mutations must share its transaction.
	plugins *plugin.Service

	// connect opens a client session to a registration; injectable so tests can
	// fake the remote server. The default implementation resolves the
	// credential for owner (bearer from the vault, OAuth via a TokenSource
	// handler) and opens the session. probeTimeout bounds one connect + tools/list.
	connect      func(ctx context.Context, reg Registration, owner CredentialOwner) (RemoteClient, error)
	probeTimeout time.Duration
	// endpoints gates registration URLs at write time and every dial; the zero
	// value is public-only.
	endpoints EndpointPolicy
	// refreshLocks serializes token refresh per (registration, owner) so
	// concurrent tool calls share one /token round trip.
	refreshLocks sync.Map
	// probePending coalesces tools/list_changed notifications so a burst of
	// them triggers one background re-probe per registration.
	probePending sync.Map
}

// defaultProbeTimeout bounds one probe (connect + tools/list).
const defaultProbeTimeout = 15 * time.Second

// NewService builds a Service. vault may be nil, in which case bearer auth is
// rejected (there is nowhere to store the secret).
func NewService(db DB, vault Vault) *Service {
	s := &Service{
		db:           db,
		vault:        vault,
		probeTimeout: defaultProbeTimeout,
	}
	s.connect = s.connectSession
	return s
}

// SetEndpointPolicy replaces the endpoint policy. Call it at startup, before
// the service handles requests; it is not synchronized.
func (s *Service) SetEndpointPolicy(policy EndpointPolicy) { s.endpoints = policy }

// NewServiceForPool builds a Service backed by the given connection pool,
// owning construction of its sqlc query set. vault may be nil, in which case
// bearer auth is rejected (there is nowhere to store the secret).
func NewServiceForPool(pool *pgxpool.Pool, vault Vault, bindVault func(pgx.Tx) Vault) *Service {
	svc := NewService(sqlc.New(pool), vault)
	svc.pool = pool
	svc.bindVault = bindVault
	return svc
}

// SetPluginService attaches the common configuration owner after startup has
// assembled both services. It must run before handling MCP requests.
func (s *Service) SetPluginService(plugins *plugin.Service) {
	if s != nil {
		s.plugins = plugins
	}
}

// SnapshotForAuthority is the read-only port used by consumers that need the
// MCP view for an agent. The common plugin service owns authority and winner
// resolution; callers never pass user or owner strings to reconstruct it.
func (s *Service) SnapshotForAuthority(ctx context.Context, authority authz.Authority, agentID string) (plugin.Snapshot, error) {
	if s == nil || s.plugins == nil {
		return plugin.Snapshot{}, errPluginCredentialsUnavailable
	}
	return s.plugins.ResolveSnapshot(ctx, authority, agentID)
}

// RegistrationsForSnapshot projects the exact enabled MCP namespace winners
// and their persisted observations for an already-authorized snapshot.
func (s *Service) RegistrationsForSnapshot(ctx context.Context, snapshot plugin.Snapshot) ([]Registration, error) {
	authority := snapshot.Authority()
	if !authority.Valid() {
		return nil, authz.ErrForbidden
	}
	observations, err := s.observationsForSnapshot(ctx, snapshot, authority)
	if err != nil {
		return nil, err
	}
	return mcpRegistrationsFromSnapshot(snapshot, observations, authority)
}

// requireCommon is the runtime cutover guard. MCP registrations have one
// durable owner now: the common plugin catalog. No production entry point may
// silently fall back to the retired registration table.
func (s *Service) requireCommon() error {
	if s == nil || s.pool == nil || s.plugins == nil {
		return errPluginCredentialsUnavailable
	}
	return nil
}

// commonRegistration resolves one authored MCP config through the common
// catalog. This is the management read path; it never consults legacy rows.
func (s *Service) commonRegistration(ctx context.Context, authority authz.Authority, id string) (Registration, error) {
	if s == nil || s.pool == nil || s.plugins == nil || !authority.Valid() {
		return Registration{}, errPluginCredentialsUnavailable
	}
	access, err := s.plugins.Begin(authority)
	if err != nil {
		return Registration{}, err
	}
	var pluginID string
	if err := s.pool.QueryRow(ctx, `SELECT plugin_id FROM plugin_config WHERE id = $1::uuid`, id).Scan(&pluginID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Registration{}, authz.ErrNotFound
		}
		return Registration{}, fmt.Errorf("mcp: read common registration identity: %w", err)
	}
	cfg, err := access.GetConfig(ctx, pluginID, id)
	if err != nil {
		return Registration{}, err
	}
	def, err := access.GetDefinition(ctx, pluginID)
	if err != nil {
		return Registration{}, err
	}
	observation, err := s.commonObservation(ctx, cfg, authority)
	if err != nil {
		return Registration{}, err
	}
	return s.registrationFromCommonConfig(ctx, authority, def, cfg, observation)
}

// commonObservation loads the exact observation owner for one resolved config.
// Shared rows use the NULL owner tuple; per-user rows use only the caller's
// trusted authority user. A missing or stale row remains StatusUnknown.
func (s *Service) commonObservation(ctx context.Context, cfg plugin.Config, authority authz.Authority) (PluginMCPObservation, error) {
	observation := PluginMCPObservation{ConfigRevision: cfg.Revision}
	payload, err := decodeMCPPluginPayload(cfg.Payload)
	if err != nil {
		return observation, err
	}
	var userID *string
	if payload.CredentialMode == CredentialModePerUser {
		if authority.Kind() != authz.ActorUser && authority.Kind() != authz.ActorAgent {
			return observation, nil
		}
		value := string(authority.UserID())
		if value == "" {
			return observation, nil
		}
		userID = &value
	}
	states, err := appdb.ListMCPConnectionStatesForConfigs(ctx, s.pool, []string{cfg.ID}, userID)
	if err != nil {
		return observation, err
	}
	for _, state := range states {
		if payload.CredentialMode == CredentialModePerUser {
			if state.CredentialUserID == nil || userID == nil || *state.CredentialUserID != *userID {
				continue
			}
		} else if state.CredentialUserID != nil {
			continue
		}
		var tools []CatalogTool
		if len(state.Tools) != 0 {
			if err := json.Unmarshal(state.Tools, &tools); err != nil {
				return observation, fmt.Errorf("mcp: decode common observation: %w", err)
			}
		}
		observation = PluginMCPObservation{
			Status: state.Status, StatusError: state.StatusError,
			ProbedAt: derefTime(state.ProbedAt), ConfigRevision: state.ConfigRevision,
			CredentialUserID: derefString(state.CredentialUserID), Tools: tools,
		}
		break
	}
	return observation, nil
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (s *Service) commonRegistrationsByScope(ctx context.Context, authority authz.Authority, scope, agentID string) ([]Registration, error) {
	if s == nil || s.pool == nil || s.plugins == nil || !authority.Valid() {
		return nil, errPluginCredentialsUnavailable
	}
	access, err := s.plugins.Begin(authority)
	if err != nil {
		return nil, err
	}
	defs, err := access.ListDefinitions(ctx)
	if err != nil {
		return nil, err
	}
	configs := make([]plugin.Config, 0)
	defsByID := make(map[string]plugin.Definition)
	for _, def := range defs {
		if def.Backend != plugin.BackendMCP {
			continue
		}
		rows, err := access.ListConfigs(ctx, def.ID, plugin.Scope(scope), agentID)
		if err != nil {
			return nil, err
		}
		for _, cfg := range rows {
			if len(cfg.Payload) == 0 {
				continue // negative config disables this scope and has no registration
			}
			configs = append(configs, cfg)
			defsByID[def.ID] = def
		}
	}
	if len(configs) == 0 {
		return []Registration{}, nil
	}
	ids := make([]string, 0, len(configs))
	perUser := false
	for _, cfg := range configs {
		ids = append(ids, cfg.ID)
		payload, err := decodeMCPPluginPayload(cfg.Payload)
		if err != nil {
			return nil, err
		}
		perUser = perUser || payload.CredentialMode == CredentialModePerUser
	}
	var credentialUserID *string
	if perUser && authority.Kind() == authz.ActorUser {
		userID := string(authority.UserID())
		credentialUserID = &userID
	}
	states, err := appdb.ListMCPConnectionStatesForConfigs(ctx, s.pool, ids, credentialUserID)
	if err != nil {
		return nil, err
	}
	type observationKey struct {
		configID string
		ownerID  string
	}
	stateByKey := make(map[observationKey]PluginMCPObservation, len(states))
	for _, state := range states {
		var tools []CatalogTool
		if err := json.Unmarshal(state.Tools, &tools); err != nil {
			return nil, fmt.Errorf("mcp: decode common observation: %w", err)
		}
		observedUserID := ""
		if state.CredentialUserID != nil {
			observedUserID = *state.CredentialUserID
		}
		stateByKey[observationKey{configID: state.ConfigID, ownerID: observedUserID}] = PluginMCPObservation{Status: state.Status, StatusError: state.StatusError, ProbedAt: derefTime(state.ProbedAt), ConfigRevision: state.ConfigRevision, CredentialUserID: observedUserID, Tools: tools}
	}
	out := make([]Registration, 0, len(configs))
	for _, cfg := range configs {
		payload, err := decodeMCPPluginPayload(cfg.Payload)
		if err != nil {
			return nil, err
		}
		ownerID := ""
		if payload.CredentialMode == CredentialModePerUser && authority.Kind() == authz.ActorUser {
			ownerID = string(authority.UserID())
		}
		reg, err := s.registrationFromCommonConfig(ctx, authority, defsByID[cfg.PluginID], cfg, stateByKey[observationKey{configID: cfg.ID, ownerID: ownerID}])
		if err != nil {
			return nil, err
		}
		out = append(out, reg)
	}
	return out, nil
}

func (s *Service) registrationFromCommonConfig(ctx context.Context, authority authz.Authority, def plugin.Definition, cfg plugin.Config, observations ...PluginMCPObservation) (Registration, error) {
	effective, err := plugin.Resolve(def, []plugin.Config{cfg}, cfg.UserID, cfg.AgentID)
	if err != nil {
		return Registration{}, err
	}
	observation := PluginMCPObservation{ConfigRevision: cfg.Revision}
	if len(observations) > 0 {
		observation = observations[0]
	}
	return RegistrationFromPluginConfig(def, cfg, effective, observation, authority)
}

func derefTime(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return value.UTC()
}

func (s *Service) withPluginMutation(ctx context.Context, authority authz.Authority, fn func(context.Context, *plugin.Access, pgx.Tx) error) error {
	if s == nil || s.pool == nil || s.plugins == nil {
		return errPluginCredentialsUnavailable
	}
	return s.plugins.WithMutationTx(ctx, authority, fn)
}

// CredentialMutation is the typed credential capability for one locked common
// config. It deliberately exposes no arbitrary vault name or scope tuple.
// Config edits and these credential operations therefore share one plugin
// mutation transaction and one config-row lock.
type CredentialMutation struct {
	tx     pgx.Tx
	config plugin.Config
	owner  CredentialOwner
	vault  Vault
	// configManaged is true only when the authority may mutate the config
	// owner tuple. A per-user caller may manage its own bundle, but must never
	// touch a shared client secret or claim to delete every user's bundle.
	configManaged bool
}

// withCredentialMutationTx binds an MCP credential mutation to the common
// plugin mutation transaction. The callback receives the bound Access and a
// typed credential capability, never a raw transaction or arbitrary Vault.
// The config row is locked and its complete identity is checked before the
// callback runs. The callback may update/delete the config through Access and
// clean up its credential refs before that same transaction commits.
func (s *Service) withCredentialMutationTx(ctx context.Context, authority authz.Authority, pluginID, configID string, expectedRevision int64, owner CredentialOwner, fn func(context.Context, *plugin.Access, plugin.Config, CredentialMutation) error) error {
	if expectedRevision < 1 || pluginID == "" || configID == "" || fn == nil {
		return plugin.ErrConflict
	}
	return s.withPluginMutation(ctx, authority, func(mutationCtx context.Context, access *plugin.Access, tx pgx.Tx) error {
		var lockedPluginID string
		var revision int64
		if err := tx.QueryRow(mutationCtx, `SELECT plugin_id, revision FROM plugin_config WHERE id = $1::uuid FOR UPDATE`, configID).Scan(&lockedPluginID, &revision); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return plugin.ErrNotFound
			}
			return fmt.Errorf("mcp: lock plugin config for credential mutation: %w", err)
		}
		if lockedPluginID != pluginID || revision != expectedRevision {
			return plugin.ErrConflict
		}
		config, err := access.GetConfig(mutationCtx, pluginID, configID)
		if err != nil {
			return err
		}
		if err := commonCredentialOwner(config, owner, authority); err != nil {
			return err
		}
		var vault Vault
		if s.bindVault != nil {
			vault = s.bindVault(tx)
		}
		managed := authority.IsAdmin() ||
			(config.Scope == plugin.ScopeUser && config.UserID == string(authority.UserID())) ||
			(config.Scope == plugin.ScopeUserAgent && config.UserID == string(authority.UserID()))
		return fn(mutationCtx, access, config, CredentialMutation{tx: tx, config: config, owner: owner, vault: vault, configManaged: managed})
	})
}

// DeleteAll removes this authorized config's entire reserved credential family,
// including grants from older modes. It cannot accept a name or owner supplied
// by a caller. The bound transaction also owns the config lock and mutation.
// Disable, reset, and individual disconnect never use this capability.
func (m CredentialMutation) DeleteAll(ctx context.Context) error {
	if !m.configManaged || m.tx == nil {
		return authz.ErrForbidden
	}
	id, err := uuid.Parse(m.config.ID)
	if err != nil {
		return authz.ErrForbidden
	}
	return vaultpkg.DeleteMCPConfigCredentialsTx(ctx, m.tx, id)
}

// StoreBearer stores the configured bearer credential at the locked config's
// owner tuple. Empty values are rejected so a caller cannot turn a write into
// an accidental credential deletion.
func (m CredentialMutation) StoreBearer(ctx context.Context, token string) error {
	payload, err := decodeMCPPluginPayload(m.config.Payload)
	if err != nil || payload.AuthType != AuthTypeBearer || token == "" {
		return authz.ErrForbidden
	}
	ref, _, _, err := decodeMCPPluginCredentialRefs(m.config.CredentialRefs, m.config, payload.AuthType, payload.CredentialMode)
	if err != nil {
		return authz.ErrForbidden
	}
	return m.storeScoped(ctx, m.owner, ref, token)
}

// StoreOAuthBundle stores a token bundle at the exact credential owner. The
// bundle name is derived from the config UUID and cannot be supplied by the
// caller.
func (m CredentialMutation) StoreOAuthBundle(ctx context.Context, bundle OAuthBundle) error {
	payload, err := decodeMCPPluginPayload(m.config.Payload)
	if err != nil || payload.AuthType != AuthTypeOAuth {
		return authz.ErrForbidden
	}
	if _, _, _, err := decodeMCPPluginCredentialRefs(m.config.CredentialRefs, m.config, payload.AuthType, payload.CredentialMode); err != nil {
		return authz.ErrForbidden
	}
	return writeOAuthBundle(ctx, m.vault, m.owner, m.config.ID, bundle)
}

// storeOAuthClientSecret stores the configured client secret at the config
// owner tuple. It is separate from StoreOAuthBundle because per-user bundles
// must never move the shared client secret to a user vault tuple.
func (m CredentialMutation) storeOAuthClientSecret(ctx context.Context, secret string) error {
	if !m.configManaged {
		return authz.ErrForbidden
	}
	payload, err := decodeMCPPluginPayload(m.config.Payload)
	if err != nil || payload.AuthType != AuthTypeOAuth || secret == "" {
		return authz.ErrForbidden
	}
	_, _, ref, err := decodeMCPPluginCredentialRefs(m.config.CredentialRefs, m.config, payload.AuthType, payload.CredentialMode)
	if err != nil || ref == "" {
		return authz.ErrForbidden
	}
	owner := CredentialOwner{Scope: string(m.config.Scope), UserID: m.config.UserID, AgentID: m.config.AgentID}
	return m.storeScoped(ctx, owner, ref, secret)
}

// DeleteOAuthBundle removes only the bundle for this trusted credential owner.
// It is safe for a per-user caller because it cannot address another user.
func (m CredentialMutation) DeleteOAuthBundle(ctx context.Context) error {
	payload, err := decodeMCPPluginPayload(m.config.Payload)
	if err != nil || payload.AuthType != AuthTypeOAuth {
		return authz.ErrForbidden
	}
	if _, _, _, err := decodeMCPPluginCredentialRefs(m.config.CredentialRefs, m.config, payload.AuthType, payload.CredentialMode); err != nil {
		return authz.ErrForbidden
	}
	return m.deleteScoped(ctx, m.owner, oauthBundleName(m.config.ID))
}

// DeleteOAuthClientSecret removes the config-owned client secret. It requires
// config-management authority, so a per-user OAuth caller cannot delete a
// system config's shared client secret.
func (m CredentialMutation) DeleteOAuthClientSecret(ctx context.Context) error {
	if !m.configManaged {
		return authz.ErrForbidden
	}
	payload, err := decodeMCPPluginPayload(m.config.Payload)
	if err != nil || payload.AuthType != AuthTypeOAuth {
		return authz.ErrForbidden
	}
	_, _, ref, err := decodeMCPPluginCredentialRefs(m.config.CredentialRefs, m.config, payload.AuthType, payload.CredentialMode)
	if err != nil || ref == "" {
		return authz.ErrForbidden
	}
	return m.deleteScoped(ctx, CredentialOwner{Scope: string(m.config.Scope), UserID: m.config.UserID, AgentID: m.config.AgentID}, ref)
}

func (m CredentialMutation) deleteScoped(ctx context.Context, owner CredentialOwner, name string) error {
	if name == "" || m.vault == nil {
		return errPluginCredentialsUnavailable
	}
	if IsSystemScope(owner.Scope) {
		return m.vault.DeleteSystemScoped(ctx, owner.Scope, owner.AgentID, name)
	}
	return m.vault.DeleteScoped(ctx, owner.Scope, owner.UserID, owner.AgentID, name)
}

func (m CredentialMutation) storeScoped(ctx context.Context, owner CredentialOwner, name, value string) error {
	if name == "" || value == "" || m.vault == nil {
		return errPluginCredentialsUnavailable
	}
	if IsSystemScope(owner.Scope) {
		return m.vault.SetSystemScoped(ctx, owner.Scope, owner.AgentID, name, value)
	}
	return m.vault.SetScoped(ctx, owner.Scope, owner.UserID, owner.AgentID, name, value)
}

// DeleteCommonConfig deletes one config and its config-owned credentials in a
// single common mutation transaction. Per-user configs are rejected because a
// single caller cannot enumerate and revoke every user's OAuth bundle safely.
func (s *Service) DeleteCommonConfig(ctx context.Context, authority authz.Authority, pluginID, configID string, expectedRevision int64, owner CredentialOwner) error {
	return s.withCredentialMutationTx(ctx, authority, pluginID, configID, expectedRevision, owner, func(mutationCtx context.Context, access *plugin.Access, _ plugin.Config, mutation CredentialMutation) error {
		return access.DeleteConfig(mutationCtx, pluginID, configID, expectedRevision)
	})
}

// ResetCommonBuiltinConfig restores the authored builtin defaults in the same
// common mutation boundary. It intentionally leaves OAuth grants untouched.
func (s *Service) ResetCommonBuiltinConfig(ctx context.Context, authority authz.Authority, pluginID, configID string, expectedRevision int64) (plugin.Config, error) {
	var reset plugin.Config
	err := s.withPluginMutation(ctx, authority, func(mutationCtx context.Context, access *plugin.Access, _ pgx.Tx) error {
		var err error
		reset, err = access.ResetBuiltinConfig(mutationCtx, pluginID, configID, expectedRevision)
		return err
	})
	return reset, err
}

func commonCredentialOwner(config plugin.Config, owner CredentialOwner, authority authz.Authority) error {
	if !authority.Valid() || authority.Kind() != authz.ActorUser {
		return authz.ErrForbidden
	}
	payload, err := decodeMCPPluginPayload(config.Payload)
	if err != nil {
		return authz.ErrForbidden
	}
	if payload.CredentialMode == CredentialModePerUser {
		// The client secret is config-owned even when user grants are
		// per-user. An administrator may initialize that shared secret at the
		// system tuple; ordinary callers may only mutate their own user bundle.
		if authority.IsAdmin() && owner.Scope == string(config.Scope) && owner.UserID == config.UserID && owner.AgentID == config.AgentID {
			return nil
		}
		if owner.Scope != ScopeUser || owner.UserID == "" || owner.UserID != string(authority.UserID()) {
			return authz.ErrForbidden
		}
		if owner.AgentID != "" {
			return authz.ErrForbidden
		}
		return nil
	}
	if config.Scope == plugin.ScopeUser || config.Scope == plugin.ScopeUserAgent {
		if owner.Scope != string(config.Scope) || owner.UserID == "" || owner.UserID != config.UserID {
			return authz.ErrForbidden
		}
		if config.Scope == plugin.ScopeUser && owner.AgentID != "" {
			return authz.ErrForbidden
		}
		if config.Scope == plugin.ScopeUserAgent && owner.AgentID != config.AgentID {
			return authz.ErrForbidden
		}
		return nil
	}
	if !authority.IsAdmin() || owner.Scope != string(config.Scope) || owner.UserID != config.UserID || owner.AgentID != config.AgentID {
		return authz.ErrForbidden
	}
	return nil
}

// CredentialOwner resolves the vault tuple holding the credential the caller
// should use: the registration's own scope for shared mode, the calling user's
// user scope for per_user mode.
func (s *Service) CredentialOwner(reg Registration, userID string) CredentialOwner {
	return credentialOwnerFor(reg, userID)
}

func credentialOwnerFor(reg Registration, userID string) CredentialOwner {
	if reg.CredentialMode == CredentialModePerUser {
		return CredentialOwner{Scope: ScopeUser, UserID: userID}
	}
	return CredentialOwner{Scope: reg.Scope, UserID: reg.UserID, AgentID: reg.AgentID}
}

// connectClient resolves the registration's credential for owner and opens a
// session. It is the lazy path tool proxies take on first Execute.
func (s *Service) connectClient(ctx context.Context, reg Registration, owner CredentialOwner) (RemoteClient, error) {
	return s.connect(ctx, reg, owner)
}

// connectSession is the default connect implementation.
func (s *Service) connectSession(ctx context.Context, reg Registration, owner CredentialOwner) (RemoteClient, error) {
	transport, err := s.buildTransport(ctx, reg, owner)
	if err != nil {
		return nil, connectionError(reg, err)
	}
	c := mcpsdk.NewClient(clientImpl, &mcpsdk.ClientOptions{
		// tools/list_changed: refresh the persisted catalog in the background so
		// the next session start picks up the server's new tools.
		ToolListChangedHandler: func(context.Context, *mcpsdk.ToolListChangedRequest) {
			s.scheduleProbe(reg, owner)
		},
	})
	session, err := c.Connect(ctx, transport, nil)
	if err != nil {
		return nil, connectionError(reg, err)
	}
	return &Client{session: session}, nil
}

// probeDebounceDelay coalesces tools/list_changed bursts into one re-probe.
const probeDebounceDelay = 5 * time.Second

func (s *Service) scheduleProbe(reg Registration, owner CredentialOwner) {
	if _, loaded := s.probePending.LoadOrStore(reg.ID, struct{}{}); loaded {
		return
	}
	time.AfterFunc(probeDebounceDelay, func() {
		s.probePending.Delete(reg.ID)
		ctx, cancel := context.WithTimeout(context.Background(), s.probeTimeout)
		defer cancel()
		// The callback captures the immutable registration and revision at the
		// time the notification was received. Probe's common observation write
		// fences that snapshot against a later config mutation.
		_, _ = s.Probe(ctx, reg, owner)
	})
}

// CreateInput describes a new registration. Token is the raw bearer token,
// stored encrypted in the vault and never persisted in the config payload.
type CreateInput struct {
	EnabledSet  bool
	Enabled     *bool
	Metadata    map[string]any
	Description *string
	// PluginID identifies the common MCP definition. It is required once the
	// common service is attached.
	PluginID       string
	Scope          string
	UserID         string
	AgentID        string
	Name           string
	URL            string
	Transport      string
	AuthType       string
	Token          string
	CredentialMode string
	// OAuthClientID is the public pre-registered client id, stored in
	// metadata.oauth.client_id. OAuthClientSecret goes to the vault, never to
	// the table. Both apply only when AuthType is oauth.
	OAuthClientID     string
	OAuthClientSecret string
	// Registry provenance, written into metadata.registry when the install
	// comes from the marketplace.
	RegistrySource  string
	RegistryID      string
	RegistryVersion string
}

func boolPtr(value bool) *bool { return &value }

// UpdateInput describes a partial registration update. Nil fields keep the
// current value; Token nil keeps the current bearer token, while Token != nil
// replaces it.
type UpdateInput struct {
	EnabledSet        bool
	Metadata          *map[string]any
	Description       *string
	ID                string
	Scope             string
	UserID            string
	AgentID           string
	NewScope          *string
	NewUserID         string
	NewAgentID        string
	Name              *string
	URL               *string
	Transport         *string
	AuthType          *string
	Enabled           *bool
	Token             *string
	CredentialMode    *string
	OAuthClientID     *string
	OAuthClientSecret *string
	ExpectedVersion   string // tool-only opaque version; empty preserves HTTP's unconditional contract
}

// Create validates the input, stores any bearer token in the vault, and inserts
// the registration. Enum validation (scope, transport, auth) happens here so the
// stdio transport and other invalid values are rejected before touching the DB.
func (s *Service) Create(ctx context.Context, in CreateInput) (Registration, error) {
	if err := s.requireCommon(); err != nil {
		return Registration{}, err
	}
	return s.createCommon(ctx, in)
}

// CreateCustom creates one custom MCP definition and its first scope config.
// The definition and config receive the same UUID, and the final credential
// refs plus encrypted secret are written before the common mutation commits.
// This seam is intentionally typed for the eventual unified API; HTTP callers
// must not pass raw secrets until their backend mutation contract is enabled.
func (s *Service) CreateCustom(ctx context.Context, def plugin.Definition, in CreateInput) (plugin.Definition, plugin.Config, error) {
	if err := s.requireCommon(); err != nil {
		return plugin.Definition{}, plugin.Config{}, err
	}
	if def.Backend != plugin.BackendMCP {
		return plugin.Definition{}, plugin.Config{}, fmt.Errorf("%w: mcp: custom definition backend must be %q", plugin.ErrInvalidConfig, plugin.BackendMCP)
	}
	if in.PluginID != "" {
		return plugin.Definition{}, plugin.Config{}, fmt.Errorf("%w: "+"mcp: custom creation must not specify plugin_id", plugin.ErrInvalidConfig)
	}
	if in.Name == "" {
		in.Name = def.DisplayName
	}
	if in.Name != def.DisplayName {
		return plugin.Definition{}, plugin.Config{}, fmt.Errorf("%w: "+"mcp: custom definition display name and config name must match", plugin.ErrInvalidConfig)
	}
	if in.Transport == "" {
		in.Transport = TransportStreamableHTTP
	}
	if in.AuthType == "" {
		in.AuthType = AuthTypeNone
	}
	if in.CredentialMode == "" {
		in.CredentialMode = CredentialModeShared
	}
	if err := validateRegistration(in.Scope, in.Name, in.URL, in.Transport, in.AuthType, s.endpoints); err != nil {
		return plugin.Definition{}, plugin.Config{}, err
	}
	if err := validateCredentialMode(in.CredentialMode, in.AuthType); err != nil {
		return plugin.Definition{}, plugin.Config{}, err
	}
	if (in.OAuthClientID != "" || in.OAuthClientSecret != "") && in.AuthType != AuthTypeOAuth {
		return plugin.Definition{}, plugin.Config{}, fmt.Errorf("%w: mcp: OAuth client credentials require auth_type %q", plugin.ErrInvalidConfig, AuthTypeOAuth)
	}
	if in.Token != "" && in.AuthType != AuthTypeBearer {
		return plugin.Definition{}, plugin.Config{}, fmt.Errorf("%w: mcp: token requires auth_type %q", plugin.ErrInvalidConfig, AuthTypeBearer)
	}
	if in.AuthType == AuthTypeBearer && strings.TrimSpace(in.Token) == "" {
		return plugin.Definition{}, plugin.Config{}, fmt.Errorf("%w: "+"mcp: bearer auth requires a token", plugin.ErrInvalidConfig)
	}
	authority, ok := authz.AuthorityFromContext(ctx)
	if !ok {
		return plugin.Definition{}, plugin.Config{}, authz.ErrUnauthenticated
	}
	// Credential locators are derived from the verified authority and the
	// requested scope. Never let a caller-supplied owner tuple reach refs.
	switch in.Scope {
	case ScopeSystem:
		in.UserID, in.AgentID = "", ""
	case ScopeSystemAgent:
		in.UserID = ""
	case ScopeUser:
		in.UserID, in.AgentID = string(authority.UserID()), ""
	case ScopeUserAgent:
		in.UserID = string(authority.UserID())
	}

	initialPayload, err := json.Marshal(map[string]any{
		"url": in.URL, "transport": in.Transport, "auth_type": AuthTypeNone,
		"credential_mode": CredentialModeShared,
	})
	if err != nil {
		return plugin.Definition{}, plugin.Config{}, err
	}
	var createdDef plugin.Definition
	var createdConfig plugin.Config
	err = s.withPluginMutation(ctx, authority, func(mutationCtx context.Context, access *plugin.Access, tx pgx.Tx) error {
		var err error
		createdDef, createdConfig, err = access.CreateCustom(mutationCtx, def, plugin.Config{
			Scope:          plugin.Scope(in.Scope),
			AgentID:        in.AgentID,
			Enabled:        createInputEnabled(in),
			Payload:        initialPayload,
			CredentialRefs: json.RawMessage(`{}`),
		})
		if err != nil {
			return err
		}
		payload, refs, err := commonMCPCreatePayload(createdConfig.ID, in)
		if err != nil {
			return err
		}
		updated, err := updateCredentialConfig(mutationCtx, access, tx, createdConfig, false, plugin.ConfigPatch{
			PayloadSet: true, Payload: payload, CredentialRefsSet: true, CredentialRefs: refs,
		})
		if err != nil {
			return err
		}
		owner := CredentialOwner{Scope: string(updated.Scope), UserID: updated.UserID, AgentID: updated.AgentID}
		vault := Vault(nil)
		if s.bindVault != nil {
			vault = s.bindVault(tx)
		}
		mutation := CredentialMutation{tx: tx, config: updated, owner: owner, vault: vault, configManaged: true}
		if in.Token != "" {
			if err := mutation.StoreBearer(mutationCtx, in.Token); err != nil {
				return err
			}
		}
		if in.OAuthClientSecret != "" {
			if err := mutation.storeOAuthClientSecret(mutationCtx, in.OAuthClientSecret); err != nil {
				return err
			}
		}
		createdConfig = updated
		return nil
	})
	if err != nil {
		return plugin.Definition{}, plugin.Config{}, err
	}
	return createdDef, createdConfig, nil
}

func (s *Service) createCommon(ctx context.Context, in CreateInput) (Registration, error) {
	if in.PluginID == "" {
		return Registration{}, fmt.Errorf("%w: mcp: plugin_id is required for common MCP config", plugin.ErrInvalidConfig)
	}
	if in.Transport == "" {
		in.Transport = TransportStreamableHTTP
	}
	if in.AuthType == "" {
		in.AuthType = AuthTypeNone
	}
	if in.CredentialMode == "" {
		in.CredentialMode = CredentialModeShared
	}
	if err := validateRegistration(in.Scope, "common", in.URL, in.Transport, in.AuthType, s.endpoints); err != nil {
		return Registration{}, err
	}
	if err := validateCredentialMode(in.CredentialMode, in.AuthType); err != nil {
		return Registration{}, err
	}
	if (in.OAuthClientID != "" || in.OAuthClientSecret != "") && in.AuthType != AuthTypeOAuth {
		return Registration{}, fmt.Errorf("%w: mcp: OAuth client credentials require auth_type %q", plugin.ErrInvalidConfig, AuthTypeOAuth)
	}
	if in.Token != "" && in.AuthType != AuthTypeBearer {
		return Registration{}, fmt.Errorf("%w: mcp: token requires auth_type %q", plugin.ErrInvalidConfig, AuthTypeBearer)
	}
	if in.AuthType == AuthTypeBearer && strings.TrimSpace(in.Token) == "" {
		return Registration{}, fmt.Errorf("%w: "+"mcp: bearer auth requires a token", plugin.ErrInvalidConfig)
	}
	if err := validateScopeOwner(in.Scope, in.UserID, in.AgentID); err != nil {
		return Registration{}, err
	}
	authority, ok := authz.AuthorityFromContext(ctx)
	if !ok {
		return Registration{}, authz.ErrUnauthenticated
	}
	return s.createCommonForAuthority(ctx, authority, in)
}

func (s *Service) createCommonForAuthority(ctx context.Context, authority authz.Authority, in CreateInput) (Registration, error) {
	var result Registration
	err := s.withPluginMutation(ctx, authority, func(mutationCtx context.Context, access *plugin.Access, tx pgx.Tx) error {
		def, err := access.GetDefinition(mutationCtx, in.PluginID)
		if err != nil {
			return err
		}
		initialPayload, err := json.Marshal(map[string]any{
			"url": in.URL, "transport": in.Transport, "auth_type": AuthTypeNone,
			"credential_mode": CredentialModeShared,
		})
		if err != nil {
			return err
		}
		created, err := access.CreateConfig(mutationCtx, plugin.Config{
			PluginID: in.PluginID, Namespace: def.Namespace, Scope: plugin.Scope(in.Scope),
			UserID: in.UserID, AgentID: in.AgentID, Enabled: createInputEnabled(in),
			Payload: initialPayload, CredentialRefs: json.RawMessage(`{}`),
		})
		if err != nil {
			return err
		}
		payload, refs, err := commonMCPCreatePayload(created.ID, in)
		if err != nil {
			return err
		}
		updated, err := updateCredentialConfig(mutationCtx, access, tx, created, false, plugin.ConfigPatch{PayloadSet: true, Payload: payload, CredentialRefsSet: true, CredentialRefs: refs})
		if err != nil {
			return err
		}
		owner := CredentialOwner{Scope: in.Scope, UserID: in.UserID, AgentID: in.AgentID}
		var vault Vault
		if s.bindVault != nil {
			vault = s.bindVault(tx)
		}
		mutation := CredentialMutation{tx: tx, config: updated, owner: owner, vault: vault, configManaged: true}
		if in.Token != "" {
			if err := mutation.StoreBearer(mutationCtx, in.Token); err != nil {
				return err
			}
		}
		if in.OAuthClientSecret != "" {
			if err := mutation.storeOAuthClientSecret(mutationCtx, in.OAuthClientSecret); err != nil {
				return err
			}
		}
		effective, err := plugin.Resolve(def, []plugin.Config{updated}, updated.UserID, updated.AgentID)
		if err != nil {
			return err
		}
		result, err = RegistrationFromPluginConfig(def, updated, effective, PluginMCPObservation{ConfigRevision: updated.Revision}, authority)
		return err
	})
	return result, err
}

func commonMCPCreatePayload(id string, in CreateInput) (json.RawMessage, json.RawMessage, error) {
	metadata := map[string]any{}
	if in.Metadata != nil {
		raw, err := json.Marshal(in.Metadata)
		if err != nil {
			return nil, nil, err
		}
		if err := json.Unmarshal(raw, &metadata); err != nil {
			return nil, nil, err
		}
	}
	if in.OAuthClientID != "" {
		oauth, _ := metadata["oauth"].(map[string]any)
		if oauth == nil {
			oauth = map[string]any{}
		}
		oauth["client_id"] = in.OAuthClientID
		metadata["oauth"] = oauth
	}

	if in.RegistrySource != "" || in.RegistryID != "" || in.RegistryVersion != "" {
		metadata["registry"] = map[string]any{"source": in.RegistrySource, "id": in.RegistryID, "version": in.RegistryVersion}
	}
	object := map[string]any{"url": in.URL, "transport": in.Transport, "auth_type": in.AuthType, "credential_mode": in.CredentialMode, "metadata": metadata}
	if in.Description != nil {
		object["description"] = *in.Description
	}
	payload, err := json.Marshal(object)
	if err != nil {
		return nil, nil, err
	}
	refs := map[string]any{}
	owner := map[string]string{"scope": in.Scope, "user_id": in.UserID, "agent_id": in.AgentID}
	if in.AuthType == AuthTypeBearer {
		refs["bearer"] = map[string]any{"name": credentialName(id), "scope": in.Scope, "user_id": in.UserID, "agent_id": in.AgentID}
	}
	if in.AuthType == AuthTypeOAuth {
		bundle := map[string]any{"name": oauthBundleName(id), "mode": in.CredentialMode}
		if in.CredentialMode == CredentialModePerUser {
			bundle["owner"] = "per_user"
		} else {
			bundle["scope"], bundle["user_id"], bundle["agent_id"] = owner["scope"], owner["user_id"], owner["agent_id"]
		}
		refs["oauth_bundle"] = bundle
		if in.OAuthClientSecret != "" {
			refs["oauth_client_secret"] = map[string]any{"name": oauthClientSecretName(id), "scope": in.Scope, "user_id": in.UserID, "agent_id": in.AgentID}
		}
	}
	refsRaw, err := json.Marshal(refs)
	return payload, refsRaw, err
}

// Get returns one registration only when it belongs to the requested scope and
// owner. Callers can map a mismatch to not-found without leaking another scope.
func (s *Service) Get(ctx context.Context, id, scope, userID, agentID string) (Registration, error) {
	if err := s.requireCommon(); err != nil {
		return Registration{}, err
	}
	authority, ok := authz.AuthorityFromContext(ctx)
	if !ok {
		return Registration{}, authz.ErrUnauthenticated
	}
	reg, err := s.commonRegistration(ctx, authority, id)
	if err != nil {
		return Registration{}, err
	}
	if reg.Scope != scope || reg.UserID != userID || reg.AgentID != agentID {
		return Registration{}, authz.ErrNotFound
	}
	return reg, nil
}

// ListByScope returns every registration in exactly one scope/owner bucket.
func (s *Service) ListByScope(ctx context.Context, scope, userID, agentID string) ([]Registration, error) {
	if err := s.requireCommon(); err != nil {
		return nil, err
	}
	authority, ok := authz.AuthorityFromContext(ctx)
	if !ok {
		return nil, authz.ErrUnauthenticated
	}
	return s.commonRegistrationsByScope(ctx, authority, scope, agentID)
}

// Update modifies a registration in the given current scope/owner bucket and
// returns the updated row. Omitted fields keep their current values. If bearer
// auth stays enabled and Token is omitted, the existing encrypted token is kept;
// scope moves require Token as the replacement for the new owner tuple.
func (s *Service) Update(ctx context.Context, in UpdateInput) (Registration, error) {
	if err := s.requireCommon(); err != nil {
		return Registration{}, err
	}
	authority, ok := authz.AuthorityFromContext(ctx)
	if !ok {
		return Registration{}, authz.ErrUnauthenticated
	}
	return s.updateCommon(ctx, authority, in, in.ExpectedVersion)
}

func (s *Service) updateCommon(ctx context.Context, authority authz.Authority, in UpdateInput, expectedVersion string) (Registration, error) {
	reg, err := s.commonRegistration(ctx, authority, in.ID)
	if err != nil {
		return Registration{}, err
	}
	if reg.Scope != in.Scope || reg.UserID != in.UserID || reg.AgentID != in.AgentID {
		return Registration{}, authz.ErrNotFound
	}
	if expectedVersion != "" && reg.Version() != expectedVersion {
		return Registration{}, ErrVersionConflict
	}
	if in.NewScope != nil || in.NewUserID != "" || in.NewAgentID != "" {
		targetScope := plugin.Scope(reg.Scope)
		targetAgentID := reg.AgentID
		if in.NewScope != nil {
			targetScope = plugin.Scope(*in.NewScope)
			if targetScope == plugin.ScopeUser || targetScope == plugin.ScopeSystem {
				targetAgentID = ""
			}
		}
		if in.NewAgentID != "" {
			targetAgentID = in.NewAgentID
		}
		targetUserID := ""
		if targetScope == plugin.ScopeUser || targetScope == plugin.ScopeUserAgent {
			targetUserID = string(authority.UserID())
		}
		if in.NewUserID != "" && in.NewUserID != targetUserID {
			return Registration{}, authz.ErrForbidden
		}
		if in.Name != nil || in.URL != nil || in.Transport != nil || in.AuthType != nil || in.Enabled != nil || in.CredentialMode != nil || in.OAuthClientID != nil || in.OAuthClientSecret != nil {
			return Registration{}, ErrScopeMoveCombinedUpdate
		}
		var replacement *string
		if in.Token != nil {
			replacement = in.Token
		}
		return s.MoveConfigScope(ctx, authority, ScopeMoveRequest{
			PluginID: reg.PluginID, ConfigID: reg.ID, ExpectedRevision: reg.ConfigRevision,
			TargetScope: targetScope, TargetAgentID: targetAgentID, Replacement: replacement,
		})
	}
	owner := s.CredentialOwner(reg, string(authority.UserID()))
	var result Registration
	err = s.withCredentialMutationTx(ctx, authority, reg.PluginID, reg.ID, reg.ConfigRevision, owner, func(mutationCtx context.Context, access *plugin.Access, current plugin.Config, mutation CredentialMutation) error {
		payload, err := decodeJSONObject(current.Payload, "MCP config payload")
		if err != nil {
			return err
		}
		currentPayload, err := decodeMCPPluginPayload(current.Payload)
		if err != nil {
			return err
		}
		authType := currentPayload.AuthType
		if in.AuthType != nil {
			authType = *in.AuthType
		}
		transport := currentPayload.Transport
		if in.Transport != nil {
			transport = *in.Transport
		}
		endpoint := currentPayload.URL
		if in.URL != nil {
			endpoint = *in.URL
		}
		mode := currentPayload.CredentialMode
		if in.CredentialMode != nil {
			mode = *in.CredentialMode
		}
		if in.Metadata != nil {
			payload["metadata"], _ = json.Marshal(*in.Metadata)
		}
		if in.Description != nil {
			payload["description"], _ = json.Marshal(*in.Description)
		}
		nextMetadata := currentPayload.Metadata
		if raw, ok := payload["metadata"]; ok {
			nextMetadata, err = decodeMCPPluginMetadata(raw)
			if err != nil {
				return fmt.Errorf("%w: %w", plugin.ErrInvalidConfig, err)
			}
		}
		nextClientID := metadataOAuthClientID(nextMetadata)
		if in.OAuthClientID != nil {
			nextClientID = *in.OAuthClientID
		}
		sensitiveEdit := endpoint != currentPayload.URL || transport != currentPayload.Transport || authType != currentPayload.AuthType || mode != currentPayload.CredentialMode || nextClientID != metadataOAuthClientID(currentPayload.Metadata) || oauthMetadataTokenEndpointAuthMethod(nextMetadata) != oauthMetadataTokenEndpointAuthMethod(currentPayload.Metadata) || in.OAuthClientSecret != nil

		if currentPayload.AuthType == AuthTypeBearer && authType == AuthTypeBearer && sensitiveEdit && in.Token == nil {
			return fmt.Errorf("%w: "+"mcp: bearer endpoint changes require a replacement token", plugin.ErrInvalidConfig)
		}
		if currentPayload.AuthType != AuthTypeBearer && authType == AuthTypeBearer && in.Token == nil {
			return fmt.Errorf("%w: "+"mcp: enabling bearer auth requires a replacement token", plugin.ErrInvalidConfig)
		}
		name := reg.Name
		if in.Name != nil {
			name = *in.Name
		}
		if err := validateRegistration(reg.Scope, name, endpoint, transport, authType, s.endpoints); err != nil {
			return err
		}
		if err := validateCredentialMode(mode, authType); err != nil {
			return err
		}
		_, _, oldClientSecretRef, err := decodeMCPPluginCredentialRefs(current.CredentialRefs, current, currentPayload.AuthType, currentPayload.CredentialMode)
		if err != nil {
			return err
		}
		if authType == AuthTypeOAuth && sensitiveEdit && oldClientSecretRef != "" && in.OAuthClientSecret == nil && nextClientID != "" {
			return fmt.Errorf("%w: "+"mcp: OAuth connection changes require a replacement client secret or clearing the client id", plugin.ErrInvalidConfig)
		}

		payload["url"], _ = json.Marshal(endpoint)
		payload["transport"], _ = json.Marshal(transport)
		payload["auth_type"], _ = json.Marshal(authType)
		payload["credential_mode"], _ = json.Marshal(mode)
		if in.OAuthClientID != nil {
			metadata := map[string]json.RawMessage{}
			if raw, ok := payload["metadata"]; ok {
				metadata, err = decodeJSONObject(raw, "MCP metadata")
				if err != nil {
					return err
				}
			}
			oauthMetadata := map[string]json.RawMessage{}
			if raw, ok := metadata["oauth"]; ok {
				oauthMetadata, err = decodeJSONObject(raw, "MCP oauth metadata")
				if err != nil {
					return err
				}
			}
			oauthMetadata["client_id"], _ = json.Marshal(*in.OAuthClientID)
			metadata["oauth"], _ = json.Marshal(oauthMetadata)
			payload["metadata"], _ = json.Marshal(metadata)
		}
		updatedPayload, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		updatedRefs, err := commonMCPUpdateRefs(current, authType, mode, in.OAuthClientSecret)
		if err != nil {
			return err
		}
		clearClientSecret := authType == AuthTypeOAuth && nextClientID == "" && in.OAuthClientSecret == nil
		if clearClientSecret {
			refs, err := decodeJSONObject(updatedRefs, "MCP credential refs")
			if err != nil {
				return err
			}
			delete(refs, "oauth_client_secret")
			updatedRefs, err = json.Marshal(refs)
			if err != nil {
				return err
			}

		}
		configPatch := plugin.ConfigPatch{PayloadSet: true, Payload: updatedPayload, CredentialRefsSet: true, CredentialRefs: updatedRefs}
		if in.Enabled != nil || in.EnabledSet {
			configPatch.EnabledSet, configPatch.Enabled = true, in.Enabled
		}
		updated, err := updateCredentialConfig(mutationCtx, access, mutation.tx, current, authType == AuthTypeOAuth && in.OAuthClientSecret != nil, configPatch)
		if err != nil {
			return err
		}
		updatedMutation := CredentialMutation{tx: mutation.tx, config: updated, owner: owner, vault: mutation.vault, configManaged: mutation.configManaged}
		if authType == AuthTypeBearer {
			if in.Token != nil {
				if err := updatedMutation.StoreBearer(mutationCtx, *in.Token); err != nil {
					return err
				}
			}
		} else if in.Token != nil {
			return fmt.Errorf("%w: "+"mcp: token requires bearer auth", plugin.ErrInvalidConfig)
		}
		if in.OAuthClientSecret != nil && *in.OAuthClientSecret != "" {
			if err := updatedMutation.storeOAuthClientSecret(mutationCtx, *in.OAuthClientSecret); err != nil {
				return err
			}
		}

		def, err := access.GetDefinition(mutationCtx, updated.PluginID)
		if err != nil {
			return err
		}
		if in.Name != nil && *in.Name != def.DisplayName {
			def, err = access.UpdateDefinition(mutationCtx, updated.PluginID, def.Revision, plugin.DefinitionPatch{DisplayName: in.Name})
			if err != nil {
				return err
			}
		}
		effective, err := plugin.Resolve(def, []plugin.Config{updated}, updated.UserID, updated.AgentID)
		if err != nil {
			return err
		}
		result, err = RegistrationFromPluginConfig(def, updated, effective, PluginMCPObservation{ConfigRevision: updated.Revision}, authority)
		return err
	})
	return result, err
}

func commonMCPUpdateRefs(current plugin.Config, authType, mode string, secret *string) (json.RawMessage, error) {
	refs := map[string]json.RawMessage{}
	if len(current.CredentialRefs) != 0 {
		var err error
		refs, err = decodeJSONObject(current.CredentialRefs, "MCP credential refs")
		if err != nil {
			return nil, err
		}
	}
	switch authType {
	case AuthTypeNone:
		refs = map[string]json.RawMessage{}
	case AuthTypeBearer:
		refs = map[string]json.RawMessage{}
		ref, _ := json.Marshal(map[string]string{"name": credentialName(current.ID), "scope": string(current.Scope), "user_id": current.UserID, "agent_id": current.AgentID})
		refs["bearer"] = ref
	case AuthTypeOAuth:
		bundle := map[string]string{"name": oauthBundleName(current.ID), "mode": mode}
		if mode == CredentialModePerUser {
			bundle["owner"] = "per_user"
		} else {
			bundle["scope"], bundle["user_id"], bundle["agent_id"] = string(current.Scope), current.UserID, current.AgentID
		}
		bundleRaw, _ := json.Marshal(bundle)
		refs["oauth_bundle"] = bundleRaw
		if secret != nil {
			if *secret == "" {
				delete(refs, "oauth_client_secret")
			} else {
				secretRaw, _ := json.Marshal(map[string]string{"name": oauthClientSecretName(current.ID), "scope": string(current.Scope), "user_id": current.UserID, "agent_id": current.AgentID})
				refs["oauth_client_secret"] = secretRaw
			}
		}
	default:
		return nil, errors.New("mcp: unsupported auth type")
	}
	return json.Marshal(refs)
}

// UpdateIfVersion applies a tool mutation only if the durable row still matches
// the opaque version observed by the caller. The SQL predicate closes the gap
// between the read and write inside this transaction.
func (s *Service) UpdateIfVersion(ctx context.Context, in UpdateInput, expectedVersion string) (Registration, error) {
	if strings.TrimSpace(expectedVersion) == "" {
		return Registration{}, ErrVersionConflict
	}
	in.ExpectedVersion = expectedVersion
	if err := s.requireCommon(); err != nil {
		return Registration{}, err
	}
	authority, ok := authz.AuthorityFromContext(ctx)
	if !ok {
		return Registration{}, authz.ErrUnauthenticated
	}
	return s.updateCommon(ctx, authority, in, expectedVersion)
}

// Delete removes a registration in the given scope and its vault credential.
func (s *Service) Delete(ctx context.Context, id, scope, userID, agentID string) error {
	if err := s.requireCommon(); err != nil {
		return err
	}
	authority, ok := authz.AuthorityFromContext(ctx)
	if !ok {
		return authz.ErrUnauthenticated
	}
	reg, err := s.commonRegistration(ctx, authority, id)
	if err != nil {
		if errors.Is(err, authz.ErrNotFound) {
			// Preserve the unconditional HTTP DELETE contract for an already
			// absent config. CAS deletion still reports a version conflict.
			return nil
		}
		return err
	}
	if reg.Scope != scope || reg.UserID != userID || reg.AgentID != agentID {
		return authz.ErrNotFound
	}
	return s.DeleteCommonConfig(ctx, authority, reg.PluginID, reg.ID, reg.ConfigRevision, s.CredentialOwner(reg, string(authority.UserID())))
}

// DeleteIfVersion is the tool-only CAS delete path. HTTP callers retain the
// existing unconditional contract through Delete.
func (s *Service) DeleteIfVersion(ctx context.Context, id, scope, userID, agentID, expectedVersion string) error {
	if strings.TrimSpace(expectedVersion) == "" {
		return ErrVersionConflict
	}
	if err := s.requireCommon(); err != nil {
		return err
	}
	authority, ok := authz.AuthorityFromContext(ctx)
	if !ok {
		return authz.ErrUnauthenticated
	}
	reg, err := s.commonRegistration(ctx, authority, id)
	if err != nil {
		if errors.Is(err, authz.ErrNotFound) {
			return ErrVersionConflict
		}
		return err
	}
	if reg.Scope != scope || reg.UserID != userID || reg.AgentID != agentID || reg.Version() != expectedVersion {
		return ErrVersionConflict
	}
	return s.DeleteCommonConfig(ctx, authority, reg.PluginID, reg.ID, reg.ConfigRevision, s.CredentialOwner(reg, string(authority.UserID())))
}

// Probe connects to the registration's endpoint, runs tools/list, and
// persists the result in the exact common observation owner and revision.
func (s *Service) Probe(ctx context.Context, reg Registration, owner CredentialOwner) (Registration, error) {
	return s.probeCommon(ctx, reg, owner)
}

// probeCommon is the observation-backed probe path. The config revision and
// exact per-user owner are fenced by StoreMCPConnectionState in the write
// transaction.
func (s *Service) probeCommon(ctx context.Context, reg Registration, owner CredentialOwner) (Registration, error) {
	if s.pool == nil || reg.ConfigRevision < 1 {
		return Registration{}, errPluginCredentialsUnavailable
	}
	// A rejected OAuth credential is terminal until CompleteOAuth supplies a
	// fresh bundle. Explicit probes must not keep retrying a known-dead refresh
	// token; CompleteOAuth builds a fresh registration without this observation.
	if reg.AuthType == AuthTypeOAuth && reg.Status == StatusNeedsAuth {
		return reg, nil
	}
	if reg.AuthType == AuthTypeOAuth && !s.OAuthState(ctx, reg, owner.UserID).Connected {
		return s.persistCommonObservation(ctx, reg, owner, StatusNeedsAuth, credentialRejectedHint, reg.Tools)
	}
	probeCtx, cancel := context.WithTimeout(ctx, s.probeTimeout)
	defer cancel()
	client, err := s.connect(probeCtx, reg, owner)
	if err != nil {
		status, reason := StatusError, probeFailedHint
		if isCredentialRejection(err) {
			status, reason = StatusNeedsAuth, credentialRejectedHint
		}
		return s.persistCommonObservation(ctx, reg, owner, status, reason, reg.Tools)
	}
	defer func() { _ = client.Close() }()
	remote, err := client.ListTools(probeCtx)
	if err != nil {
		status, reason := StatusError, probeFailedHint
		if isCredentialRejection(err) {
			status, reason = StatusNeedsAuth, credentialRejectedHint
		}
		return s.persistCommonObservation(ctx, reg, owner, status, reason, reg.Tools)
	}
	tools := make([]CatalogTool, 0, len(remote))
	for _, rt := range remote {
		tools = append(tools, CatalogTool{
			Name: rt.Name, Description: rt.Description,
			InputSchema: cloneSchema(toolInputSchema(rt.InputSchema)),
			Annotations: annotationsSchema(rt.Annotations),
		})
	}
	if err := validateCatalogTools(reg, tools); err != nil {
		return Registration{}, err
	}
	return s.persistCommonObservation(ctx, reg, owner, StatusOK, "", tools)
}

// validateCatalogTools applies the same model-facing identity rules to a
// freshly probed catalog that the snapshot converter applies to cached state.
// A duplicate after sanitization is an invalid observation, so callers must
// reject the entire catalog instead of publishing a partial tool set.
func validateCatalogTools(reg Registration, catalog []CatalogTool) error {
	seen := make(map[string]struct{}, len(catalog))
	for _, catalogTool := range catalog {
		local := SanitizeIdent(catalogTool.Name, "tool")
		name := exportedToolName(reg, catalogTool.Name)
		if reg.Namespace != "" {
			var err error
			name, err = plugin.ExportedToolName(reg.Namespace, local)
			if err != nil {
				return fmt.Errorf("mcp: invalid discovered tool name: %w", err)
			}
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("mcp: discovered catalog has duplicate exported tool name")
		}
		seen[name] = struct{}{}
	}
	return nil
}

func (s *Service) persistCommonObservation(ctx context.Context, reg Registration, owner CredentialOwner, status, reason string, tools []CatalogTool) (Registration, error) {
	if !ValidStatus(status) {
		return Registration{}, fmt.Errorf("mcp: invalid observation status %q", status)
	}
	if err := validateCredentialOwner(reg, owner); err != nil {
		return Registration{}, err
	}
	if tools == nil {
		tools = []CatalogTool{}
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		return Registration{}, fmt.Errorf("mcp: encode MCP observation: %w", err)
	}
	var credentialUserID *string
	if reg.CredentialMode == CredentialModePerUser {
		if owner.UserID == "" {
			return Registration{}, fmt.Errorf("mcp: per-user MCP observation requires a trusted user")
		}
		userID := owner.UserID
		credentialUserID = &userID
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Registration{}, fmt.Errorf("mcp: begin MCP observation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = appdb.StoreMCPConnectionState(ctx, tx, appdb.MCPConnectionState{
		ConfigID: reg.ID, CredentialUserID: credentialUserID, Tools: raw,
		Status: status, StatusError: reason, ProbedAt: timePtr(time.Now().UTC()),
		ConfigRevision: reg.ConfigRevision,
	})
	if err != nil {
		return Registration{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Registration{}, fmt.Errorf("mcp: commit MCP observation: %w", err)
	}
	reg.Status, reg.StatusError = status, reason
	reg.ProbedAt = time.Now().UTC()
	reg.Tools = tools
	return reg, nil
}

// persistCommonStatus updates only the selected observation's status. The
// parent lock and revision predicate prevent a stale OAuth response or tool
// call from changing a newer config, and IS NOT DISTINCT FROM keeps shared
// (NULL owner) state separate from per-user state.
func (s *Service) persistCommonStatus(ctx context.Context, reg Registration, owner CredentialOwner, status, reason string) error {
	if s == nil || s.pool == nil || reg.ConfigRevision < 1 {
		return errPluginCredentialsUnavailable
	}
	if err := validateCredentialOwner(reg, owner); err != nil {
		return err
	}
	var ownerID any
	if reg.CredentialMode == CredentialModePerUser {
		if owner.UserID == "" {
			return fmt.Errorf("mcp: per-user MCP status requires a trusted user")
		}
		ownerID = owner.UserID
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("mcp: begin MCP status: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var revision int64
	if err := tx.QueryRow(ctx, `SELECT revision FROM plugin_config WHERE id = $1::uuid FOR UPDATE`, reg.ID).Scan(&revision); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errPluginConfigIdentity
		}
		return fmt.Errorf("mcp: lock MCP config for status: %w", err)
	}
	if revision != reg.ConfigRevision {
		return fmt.Errorf("%w: got %d, current %d", appdb.ErrMCPConnectionStateStale, reg.ConfigRevision, revision)
	}
	result, err := tx.Exec(ctx, `
		UPDATE mcp_connection_state
		SET status = $3, status_error = $4, updated_at = now()
		WHERE config_id = $1::uuid
		  AND credential_user_id IS NOT DISTINCT FROM $2::uuid
		  AND config_revision = $5`, reg.ID, ownerID, status, reason, reg.ConfigRevision)
	if err != nil {
		return fmt.Errorf("mcp: update MCP status: %w", err)
	}
	if result.RowsAffected() == 0 {
		if _, err := appdb.StoreMCPConnectionState(ctx, tx, appdb.MCPConnectionState{
			ConfigID: reg.ID, CredentialUserID: ownerStringPtr(ownerID),
			Tools: json.RawMessage(`[]`), Status: status, StatusError: reason,
			ConfigRevision: reg.ConfigRevision,
		}); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("mcp: commit MCP status: %w", err)
	}
	return nil
}

func ownerStringPtr(owner any) *string {
	value, ok := owner.(string)
	if !ok || value == "" {
		return nil
	}
	return &value
}

func timePtr(v time.Time) *time.Time { return &v }

// SetConnectForTesting replaces the transport-level connect function with a
// fake remote. Test-only seam: real endpoints are unreachable in tests because
// the SSRF-safe dialer refuses loopback/private targets.
func (s *Service) SetConnectForTesting(fn func(ctx context.Context, reg Registration, owner CredentialOwner) (RemoteClient, error)) {
	s.connect = fn
}

// SetStatus persists a status transition without a probe (e.g. a tool call
// rejected a stored credential). It deliberately leaves updated_at alone so a
// status change cannot invalidate a client's If-Match version.
func (s *Service) SetStatus(ctx context.Context, id, status, msg string) error {
	if !ValidStatus(status) {
		return fmt.Errorf("mcp: invalid status %q", status)
	}
	return fmt.Errorf("mcp: status update requires resolved common registration and credential owner")
}

// Probe errors never cross the persistence or API boundary because endpoint
// paths are write-only input and may carry secrets. Credential rejection has
// its own stable status and hint in probeCommon.
const probeFailedHint = "MCP probe failed"

// BearerToken returns the decrypted bearer token for a registration, or an empty
// string when the server needs no auth.
func (s *Service) BearerToken(ctx context.Context, reg Registration) (string, error) {
	if reg.AuthType != AuthTypeBearer || reg.CredentialRef == "" {
		return "", nil
	}
	snapshot, err := s.loadCredentialSnapshot(ctx, reg, s.CredentialOwner(reg, reg.UserID))
	if err != nil {
		return "", err
	}
	return snapshot.BearerToken, nil
}

// validateScopeOwner enforces the same scope/owner coupling as the DB CHECK, so
// callers get a clear error before the insert instead of a constraint violation.
func validateScopeOwner(scope, userID, agentID string) error {
	switch scope {
	case ScopeUser:
		if userID == "" || agentID != "" {
			return fmt.Errorf("%w: mcp: user scope requires user_id only", plugin.ErrInvalidConfig)
		}
	case ScopeUserAgent:
		if userID == "" || agentID == "" {
			return fmt.Errorf("%w: mcp: user_agent scope requires user_id and agent_id", plugin.ErrInvalidConfig)
		}
	case ScopeSystem:
		if userID != "" || agentID != "" {
			return fmt.Errorf("%w: mcp: system scope cannot include user_id or agent_id", plugin.ErrInvalidConfig)
		}
	case ScopeSystemAgent:
		if userID != "" || agentID == "" {
			return fmt.Errorf("%w: mcp: system_agent scope requires agent_id only", plugin.ErrInvalidConfig)
		}
	default:
		return fmt.Errorf("%w: mcp: invalid scope %q", plugin.ErrInvalidConfig, scope)
	}
	return nil
}

func textOrEmpty(v pgtype.Text) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

func registrationFromRow(row sqlc.McpServer) Registration {
	var metadata map[string]any
	if len(row.Metadata) > 0 {
		_ = json.Unmarshal(row.Metadata, &metadata)
	}
	return Registration{
		ID:             row.ID,
		Scope:          row.Scope,
		UserID:         textOrEmpty(row.UserID),
		AgentID:        textOrEmpty(row.AgentID),
		Name:           row.Name,
		URL:            row.Url,
		Transport:      row.Transport,
		AuthType:       row.AuthType,
		CredentialRef:  row.CredentialRef,
		Enabled:        row.Enabled,
		Status:         row.Status,
		StatusError:    row.StatusError,
		ProbedAt:       row.ProbedAt.Time.UTC(),
		Tools:          decodeCatalog(row.Tools),
		CredentialMode: row.CredentialMode,
		Metadata:       metadata,
		OAuthClientID:  oauthClientID(metadata),
		CreatedAt:      row.CreatedAt.UTC(),
		UpdatedAt:      row.UpdatedAt.UTC(),
	}
}

// oauthClientID reads the public OAuth client id out of metadata.oauth.
func oauthClientID(metadata map[string]any) string {
	oauthMeta, _ := metadata["oauth"].(map[string]any)
	id, _ := oauthMeta["client_id"].(string)
	return id
}

// decodeCatalog unmarshals the persisted tool catalog; corrupt or empty JSON
// yields nil so callers treat the server as unprobed.
func decodeCatalog(raw json.RawMessage) []CatalogTool {
	if len(raw) == 0 {
		return nil
	}
	var out []CatalogTool
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func createInputEnabled(in CreateInput) *bool {
	if in.EnabledSet {
		return in.Enabled
	}
	return boolPtr(true)
}
