package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CherryHQ/stella/internal/authz"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
)

var (
	ErrConflict      = errors.New("plugin: revision conflict")
	ErrBuiltinConfig = errors.New("plugin: builtin system config cannot be deleted")
	ErrNotFound      = authz.ErrNotFound
)

// ConfigPatch distinguishes omitted fields from explicit null ownership. The
// persistence port always receives the resulting complete row.
type ConfigPatch struct {
	EnabledSet bool
	Enabled    *bool
	PayloadSet bool
	Payload    json.RawMessage
	// BinaryVersions is a typed write-only edit for the CLI backend. It lets
	// callers change the visible version without sending locator or options back.
	BinaryVersionsSet bool
	BinaryVersions    map[string]string
	CredentialRefsSet bool
	CredentialRefs    json.RawMessage
	ResetFields       []string
}

// PayloadValidator always checks field permissions, types, secrets and locator
// ownership. Enabled=false suppresses only completeness/readiness requirements.
// A new disabled definition is checked with a nil payload to validate its
// immutable resource schema without requiring connection fields.
// CLI system scopes may configure host installation; user scopes are confined
// to sandbox-local versions/options and runtime credentials, never system PATH,
// shared install directories or host hooks. Runtime use must validate again
// after caps lift; storing a disabled configuration is not execution approval.
type PayloadValidator func(context.Context, Definition, Config, []string) error

// BackendPolicy is the single backend boundary for plugin configuration. The
// transition runs after the SQL row mutation and before the enclosing commit.
type BackendPolicy struct {
	Validate   PayloadValidator
	Transition BackendTransition
}

type BackendTransition func(context.Context, pgx.Tx, authz.Authority, MutationKind, Definition, *Config, *Config) error

type MutationKind string

const (
	MutationCreate MutationKind = "create"
	MutationUpdate MutationKind = "update"
	MutationMove   MutationKind = "move"
	MutationReset  MutationKind = "reset"
	MutationDelete MutationKind = "delete"
)

// Service owns persistence; Access is its only caller-facing authorization boundary.
type Service struct {
	db            *pgxpool.Pool
	q             *sqlc.Queries
	agents        *agentaccess.Service
	catalog       *Catalog
	policy        BackendPolicy
	mutationTx    pgx.Tx
	mutationFence MutationFence
	txBound       bool
}

func NewService(db *pgxpool.Pool, agents *agentaccess.Service, catalog *Catalog, policy BackendPolicy, mutationFence MutationFence) *Service {
	shipped := NewCatalog()
	if catalog != nil {
		for _, def := range catalog.Definitions() {
			shipped.byID[def.ID] = cloneDefinition(def)
		}
	}
	return &Service{db: db, q: sqlc.New(db), agents: agents, catalog: shipped, policy: policy, mutationFence: mutationFence}
}

func (s *Service) Begin(authority authz.Authority) (*Access, error) {
	if s == nil || s.db == nil || !authority.Valid() || authority.Kind() != authz.ActorUser {
		return nil, ErrForbidden
	}
	return &Access{service: s, authority: authority}, nil
}

func (b *Access) GetDefinition(ctx context.Context, id string) (Definition, error) {
	if err := b.ensureActive(); err != nil {
		return Definition{}, err
	}
	def, err := b.service.getDefinition(ctx, id)
	if err != nil {
		return Definition{}, err
	}
	visible, err := b.definitionVisible(ctx, def)
	if err != nil {
		return Definition{}, err
	}
	if !visible {
		return Definition{}, ErrNotFound
	}
	return def, nil
}

func (b *Access) definitionVisible(ctx context.Context, def Definition) (bool, error) {
	if def.Source == SourceBuiltin || b.authority.IsAdmin() {
		return true, nil
	}
	rows, err := b.service.q.ListPluginConfigs(ctx, def.ID)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		config := fromSQLConfig(row)
		if config.UserID == "" && len(config.Payload) == 0 {
			continue
		}
		if config.UserID != "" && config.UserID != string(b.authority.UserID()) {
			continue
		}
		if config.AgentID != "" {
			if b.service.agents == nil {
				continue
			}
			if err := b.service.agents.Authorize(ctx, b.authority, config.AgentID, authz.ActionRead); err != nil {
				if errors.Is(err, authz.ErrForbidden) || errors.Is(err, authz.ErrNotFound) {
					continue
				}
				return false, err
			}
		}
		return true, nil
	}
	return false, nil
}

func (b *Access) ListDefinitions(ctx context.Context) ([]Definition, error) {
	if err := b.ensureActive(); err != nil {
		return nil, err
	}
	defs, err := b.service.listDefinitions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Definition, 0, len(defs))
	for _, def := range defs {
		if def.Source == SourceBuiltin {
			if _, ok := b.service.catalog.Get(def.ID); !ok {
				continue
			}
		}
		visible, err := b.definitionVisible(ctx, def)
		if err != nil {
			return nil, err
		}
		if visible {
			out = append(out, def)
		}
	}
	return out, nil
}

func (b *Access) ListConfigs(ctx context.Context, pluginID string, scope Scope, agentID string) ([]Config, error) {
	if err := b.ensureActive(); err != nil {
		return nil, err
	}
	def, err := b.GetDefinition(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	userID, ownerAgentID, err := b.owner(ctx, scope, agentID)
	if err != nil {
		return nil, err
	}
	return b.service.listConfigs(ctx, def.ID, scope, userID, ownerAgentID)
}

func (b *Access) GetConfig(ctx context.Context, pluginID, id string) (Config, error) {
	if err := b.ensureActive(); err != nil {
		return Config{}, err
	}
	row, err := b.service.q.GetPluginConfigForOwner(ctx, sqlc.GetPluginConfigForOwnerParams{
		ID: id, PluginID: pluginID, IsAdmin: b.authority.IsAdmin(), ViewerUserID: string(b.authority.UserID()),
	})
	if err != nil {
		return Config{}, mapNotFound(err)
	}
	config := fromSQLConfig(row)
	if config.PluginID != pluginID {
		return Config{}, ErrNotFound
	}
	ownerUserID, ownerAgentID, err := b.owner(ctx, config.Scope, config.AgentID)
	if err != nil {
		if errors.Is(err, authz.ErrForbidden) || errors.Is(err, authz.ErrNotFound) {
			return Config{}, ErrNotFound
		}
		return Config{}, err
	}
	if config.UserID != ownerUserID || config.AgentID != ownerAgentID {
		return Config{}, ErrNotFound
	}
	if _, err := b.service.getDefinition(ctx, pluginID); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (b *Access) CreateConfig(ctx context.Context, config Config) (Config, error) {
	if err := b.ensureActive(); err != nil {
		return Config{}, err
	}
	if !b.service.txBound {
		var created Config
		err := b.service.WithMutationTx(ctx, b.authority, func(mutationCtx context.Context, bound *Access, _ pgx.Tx) error {
			var err error
			created, err = bound.CreateConfig(mutationCtx, config)
			return err
		})
		return created, err
	}
	def, err := b.GetDefinition(ctx, config.PluginID)
	if err != nil {
		return Config{}, err
	}
	userID, agentID, err := b.owner(ctx, config.Scope, config.AgentID)
	if err != nil {
		return Config{}, err
	}
	config.PluginID, config.Namespace = def.ID, def.Namespace
	config.UserID, config.AgentID = userID, agentID
	id, err := uuid.NewV7()
	if err != nil {
		return Config{}, err
	}
	config.ID, config.Revision = id.String(), 1
	config.CredentialRefs = nonEmptyJSON(config.CredentialRefs)
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	if err := rejectImmutableSkillPayload(config.Payload); err != nil {
		return Config{}, err
	}
	if err := b.service.validateResolved(ctx, def, config, nil); err != nil {
		return Config{}, err
	}
	created, err := b.service.createConfig(ctx, config)
	if err != nil {
		return Config{}, err
	}
	if err := b.service.transitionConfig(ctx, b.authority, MutationCreate, def, nil, &created); err != nil {
		return Config{}, err
	}
	return created, nil
}

func (b *Access) UpdateConfig(ctx context.Context, pluginID, id string, expectedRevision int64, patch ConfigPatch) (Config, error) {
	if err := b.ensureActive(); err != nil {
		return Config{}, err
	}
	if !b.service.txBound {
		var updated Config
		err := b.service.WithMutationTx(ctx, b.authority, func(mutationCtx context.Context, bound *Access, _ pgx.Tx) error {
			var err error
			updated, err = bound.UpdateConfig(mutationCtx, pluginID, id, expectedRevision, patch)
			return err
		})
		return updated, err
	}
	current, err := b.GetConfig(ctx, pluginID, id)
	if err != nil {
		return Config{}, err
	}
	if expectedRevision < 1 {
		return Config{}, ErrConflict
	}
	def, err := b.GetDefinition(ctx, current.PluginID)
	if err != nil {
		return Config{}, err
	}
	if err := rejectImmutableSkillPatch(patch); err != nil {
		return Config{}, err
	}
	if patch.BinaryVersionsSet {
		typedPayload, err := applyCLIWriteOnlyPatch(def, current.Payload, patch, b.authority.IsAdmin())
		if err != nil {
			return Config{}, err
		}
		patch.PayloadSet = true
		patch.Payload = typedPayload
	}
	updated := current
	if patch.EnabledSet {
		updated.Enabled = patch.Enabled
	}
	updated.Payload, err = patchPayload(current.Payload, patch)
	if err != nil {
		return Config{}, err
	}
	if patch.CredentialRefsSet {
		updated.CredentialRefs = patch.CredentialRefs
	}
	if err := updated.Validate(); err != nil {
		return Config{}, err
	}
	closingOnly := patch.EnabledSet && patch.Enabled != nil && !*patch.Enabled &&
		!patch.PayloadSet && !patch.CredentialRefsSet && len(patch.ResetFields) == 0
	// An obsolete payload must never prevent its owner from closing it. Any
	// accompanying field write still goes through the backend safety boundary.
	if !closingOnly {
		if err := b.service.validateResolved(ctx, def, updated, patch.ResetFields); err != nil {
			return Config{}, err
		}
	}
	result, err := b.service.updateConfigCAS(ctx, current.ID, expectedRevision, updated.Enabled, updated.Payload, updated.CredentialRefs)
	if err != nil {
		return Config{}, err
	}
	if err := b.service.transitionConfig(ctx, b.authority, MutationUpdate, def, &current, &result); err != nil {
		return Config{}, err
	}
	return result, nil
}

// MoveConfig changes a config's scope while preserving its config ID. Both the
// source tuple and the target tuple are authorized through the same owner/PEP
// boundary; target user identity is always derived from the bound authority.
// The complete resulting config is validated before one CAS update changes the
// ownership tuple and payload atomically.
func (b *Access) MoveConfig(ctx context.Context, pluginID, id string, expectedRevision int64, targetScope Scope, targetAgentID string, patch ConfigPatch) (Config, error) {
	if err := b.ensureActive(); err != nil {
		return Config{}, err
	}
	if !b.service.txBound {
		var moved Config
		err := b.service.WithMutationTx(ctx, b.authority, func(mutationCtx context.Context, bound *Access, _ pgx.Tx) error {
			var err error
			moved, err = bound.MoveConfig(mutationCtx, pluginID, id, expectedRevision, targetScope, targetAgentID, patch)
			return err
		})
		return moved, err
	}
	if expectedRevision < 1 {
		return Config{}, ErrConflict
	}
	current, err := b.GetConfig(ctx, pluginID, id)
	if err != nil {
		return Config{}, err
	}
	def, err := b.GetDefinition(ctx, current.PluginID)
	if err != nil {
		return Config{}, err
	}
	if err := rejectImmutableSkillPatch(patch); err != nil {
		return Config{}, err
	}
	if def.Source == SourceBuiltin && current.Scope == ScopeSystem {
		return Config{}, ErrBuiltinConfig
	}
	// Re-check both tuples at the mutation boundary. GetConfig authorizes the
	// source tuple; owner derives and checks the destination tuple, including
	// AgentPEP access for agent scopes.
	if _, _, err := b.owner(ctx, current.Scope, current.AgentID); err != nil {
		return Config{}, err
	}
	targetUserID, resolvedTargetAgentID, err := b.owner(ctx, targetScope, targetAgentID)
	if err != nil {
		return Config{}, err
	}
	if patch.BinaryVersionsSet {
		typedPayload, err := applyCLIWriteOnlyPatch(def, current.Payload, patch, b.authority.IsAdmin())
		if err != nil {
			return Config{}, err
		}
		patch.PayloadSet = true
		patch.Payload = typedPayload
	}
	updated := current
	updated.Scope = targetScope
	updated.UserID = targetUserID
	updated.AgentID = resolvedTargetAgentID
	if patch.EnabledSet {
		updated.Enabled = patch.Enabled
	}
	updated.Payload, err = patchPayload(current.Payload, patch)
	if err != nil {
		return Config{}, err
	}
	if patch.CredentialRefsSet {
		updated.CredentialRefs = patch.CredentialRefs
	}
	if err := updated.Validate(); err != nil {
		return Config{}, err
	}
	if err := b.service.validateResolved(ctx, def, updated, patch.ResetFields); err != nil {
		return Config{}, err
	}
	result, err := b.service.moveConfigCAS(ctx, current.ID, expectedRevision, updated.Scope, updated.UserID, updated.AgentID, updated.Enabled, updated.Payload, updated.CredentialRefs)
	if err != nil {
		return Config{}, err
	}
	if err := b.service.transitionConfig(ctx, b.authority, MutationMove, def, &current, &result); err != nil {
		return Config{}, err
	}
	return result, nil
}

func (b *Access) DeleteConfig(ctx context.Context, pluginID, id string, expectedRevision int64) error {
	if err := b.ensureActive(); err != nil {
		return err
	}
	if !b.service.txBound {
		return b.service.WithMutationTx(ctx, b.authority, func(mutationCtx context.Context, bound *Access, _ pgx.Tx) error {
			return bound.DeleteConfig(mutationCtx, pluginID, id, expectedRevision)
		})
	}
	current, err := b.GetConfig(ctx, pluginID, id)
	if err != nil {
		return err
	}
	def, err := b.GetDefinition(ctx, current.PluginID)
	if err != nil {
		return err
	}
	if def.Source == SourceBuiltin && current.Scope == ScopeSystem {
		return ErrBuiltinConfig
	}
	deleted, err := b.service.deleteConfigCAS(ctx, id, expectedRevision, current.PluginID)
	if err != nil {
		return err
	}
	if !deleted {
		return ErrConflict
	}
	return b.service.transitionConfig(ctx, b.authority, MutationDelete, def, &current, nil)
}

func (b *Access) ResetBuiltinConfig(ctx context.Context, pluginID, id string, expectedRevision int64) (Config, error) {
	if err := b.ensureActive(); err != nil {
		return Config{}, err
	}
	if !b.service.txBound {
		var reset Config
		err := b.service.WithMutationTx(ctx, b.authority, func(mutationCtx context.Context, bound *Access, _ pgx.Tx) error {
			var err error
			reset, err = bound.ResetBuiltinConfig(mutationCtx, pluginID, id, expectedRevision)
			return err
		})
		return reset, err
	}
	current, err := b.GetConfig(ctx, pluginID, id)
	if err != nil {
		return Config{}, err
	}
	def, err := b.GetDefinition(ctx, current.PluginID)
	if err != nil {
		return Config{}, err
	}
	if !b.authority.IsAdmin() || def.Source != SourceBuiltin || current.Scope != ScopeSystem {
		return Config{}, ErrForbidden
	}
	reset, err := b.service.resetBuiltinConfig(ctx, id, expectedRevision, current.PluginID)
	if err != nil {
		return Config{}, err
	}
	if err := b.service.transitionConfig(ctx, b.authority, MutationReset, def, &current, &reset); err != nil {
		return Config{}, err
	}
	return reset, nil
}

// SyncBuiltinDefaults is one startup transaction. Existing config identities,
// explicit decisions, pins and timestamps are never rewritten by a release.
func (s *Service) SyncBuiltinDefaults(ctx context.Context) error {
	if s == nil || s.db == nil || s.mutationFence == nil || ctx == nil {
		return ErrForbidden
	}
	if mutationInProgress(ctx) || s.txBound {
		return ErrNestedMutation
	}
	return s.mutationFence(ctx, func() error { return s.syncBuiltinDefaultsTx(ctx) })
}

func (s *Service) syncBuiltinDefaultsTx(ctx context.Context) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlc.New(tx)
	if err := q.LockPluginCatalog(ctx); err != nil {
		return err
	}
	for _, def := range s.catalog.Definitions() {
		if def.Source != SourceBuiltin {
			continue
		}
		if err := def.Validate(); err != nil {
			return err
		}
		_, err := q.UpsertPluginDefinition(ctx, sqlc.UpsertPluginDefinitionParams{
			ID: def.ID, Namespace: def.Namespace, DisplayName: def.DisplayName,
			Backend: string(def.Backend), Source: string(def.Source), ImplementationKey: def.ImplementationKey,
			Spec: def.Spec, DefaultEnabled: def.DefaultEnabled, Revision: def.Revision,
		})
		if err != nil {
			return fmt.Errorf("sync definition %s: %w", def.ID, err)
		}
		if _, err := q.EnsureSystemPluginConfig(ctx, sqlc.EnsureSystemPluginConfigParams{PluginID: def.ID, Namespace: def.Namespace}); err != nil {
			return fmt.Errorf("sync config %s: %w", def.ID, err)
		}
	}
	return classifyCommitError(tx.Commit(ctx))
}

func (b *Access) CreateCustom(ctx context.Context, def Definition, config Config) (Definition, Config, error) {
	if err := b.ensureActive(); err != nil {
		return Definition{}, Config{}, err
	}
	if !b.service.txBound {
		var createdDef Definition
		var createdConfig Config
		err := b.service.WithMutationTx(ctx, b.authority, func(mutationCtx context.Context, bound *Access, _ pgx.Tx) error {
			var err error
			createdDef, createdConfig, err = bound.CreateCustom(mutationCtx, def, config)
			return err
		})
		return createdDef, createdConfig, err
	}
	if def.Backend == BackendCLI && !b.authority.IsAdmin() {
		return Definition{}, Config{}, ErrForbidden
	}
	if def.Backend != BackendCLI && def.Backend != BackendMCP {
		return Definition{}, Config{}, ErrInvalidDefinition
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Definition{}, Config{}, err
	}
	def.ID, def.Source, def.Revision = "custom/"+id.String(), SourceCustom, 1
	def.ImplementationKey, def.CreatorUserID, def.DefaultEnabled = string(def.Backend), string(b.authority.UserID()), false
	if b.authority.IsAdmin() {
		def.CreatorUserID = ""
	}
	def.Spec = nonEmptyJSON(def.Spec)
	if err := validateCustomSpec(def); err != nil {
		return Definition{}, Config{}, err
	}
	userID, agentID, err := b.owner(ctx, config.Scope, config.AgentID)
	if err != nil {
		return Definition{}, Config{}, err
	}
	config.ID, config.PluginID, config.Namespace = id.String(), def.ID, def.Namespace
	config.UserID, config.AgentID, config.Revision = userID, agentID, 1
	config.CredentialRefs = nonEmptyJSON(config.CredentialRefs)
	if err := config.Validate(); err != nil {
		return Definition{}, Config{}, err
	}
	if err := rejectImmutableSkillPayload(config.Payload); err != nil {
		return Definition{}, Config{}, err
	}
	if config.Enabled == nil || !*config.Enabled {
		if b.service.policy.Validate == nil {
			return Definition{}, Config{}, ErrInvalidDefinition
		}
		definitionOnly := cloneConfig(config)
		disabled := false
		definitionOnly.Payload, definitionOnly.Enabled = nil, &disabled
		if err := b.service.policy.Validate(ctx, def, definitionOnly, nil); err != nil {
			return Definition{}, Config{}, err
		}
	}
	if err := b.service.validateResolved(ctx, def, config, nil); err != nil {
		return Definition{}, Config{}, err
	}
	createdDef, createdConfig, err := b.service.createCustom(ctx, def, config)
	if err != nil {
		return Definition{}, Config{}, err
	}
	if err := b.service.transitionConfig(ctx, b.authority, MutationCreate, createdDef, nil, &createdConfig); err != nil {
		return Definition{}, Config{}, err
	}
	return createdDef, createdConfig, nil
}

// An MCP definition has no shared endpoint/auth base. CLI resource fields are
// the same explicit schema projected by its release loader; backend validation
// additionally checks their typed contents before any config is persisted.
func validateCustomSpec(def Definition) error {
	if err := def.Validate(); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(def.Spec, &fields); err != nil {
		return ErrInvalidDefinition
	}
	for field, value := range fields {
		if field == "description" {
			var description string
			if err := json.Unmarshal(value, &description); err != nil {
				return ErrInvalidDefinition
			}
			continue
		}
		if def.Backend == BackendCLI {
			switch field {
			case "category", "prompt", "binaries", "session_env", "oauth_provider":
				continue
			case "skills":
				var skills []json.RawMessage
				if err := json.Unmarshal(value, &skills); err != nil || len(skills) != 0 {
					return fmt.Errorf("%w: custom CLI definitions cannot claim bundled skills", ErrInvalidDefinition)
				}
				continue
			}
		}
		return fmt.Errorf("%w: unsupported definition field %s", ErrInvalidDefinition, field)
	}
	return nil
}

func (s *Service) validateResolved(ctx context.Context, def Definition, config Config, resetFields []string) error {
	if err := config.Validate(); err != nil {
		return err
	}
	enabled := def.DefaultEnabled
	if config.Enabled != nil {
		enabled = *config.Enabled
	}
	if len(config.Payload) == 0 {
		return nil
	}
	if err := rejectImmutableSkillPayload(config.Payload); err != nil {
		return err
	}
	// Only fixed upper bounds can suppress validation here: user configs may
	// later execute on many Agents, so an unrelated Agent cap is irrelevant.
	for _, scope := range []Scope{ScopeSystem, ScopeSystemAgent} {
		if config.Scope == scope {
			continue
		}
		agentID := ""
		if scope == ScopeSystemAgent {
			if config.AgentID == "" {
				continue
			}
			agentID = config.AgentID
		}
		rows, err := s.q.ListPluginConfigsOwned(ctx, sqlc.ListPluginConfigsOwnedParams{
			PluginID: def.ID, Scope: string(scope), AgentID: nullableText(agentID),
		})
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Enabled.Valid && !row.Enabled.Bool {
				enabled = false
			}
		}
	}
	merged, err := mergeObjects(def.Spec, config.Payload)
	if err != nil {
		return fmt.Errorf("%w: resolved payload: %w", ErrInvalidConfig, err)
	}
	if s.policy.Validate == nil {
		return fmt.Errorf("%w: backend validator unavailable", ErrInvalidConfig)
	}
	resolved := cloneConfig(config)
	resolved.Payload, resolved.Enabled = merged, &enabled
	return s.policy.Validate(ctx, def, resolved, resetFields)
}

func rejectImmutableSkillPayload(raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return fmt.Errorf("%w: payload must be a JSON object", ErrInvalidConfig)
	}
	if _, exists := fields["skills"]; exists {
		return fmt.Errorf("%w: bundled skill membership is immutable", ErrInvalidConfig)
	}
	return nil
}

func rejectImmutableSkillPatch(patch ConfigPatch) error {
	if slices.Contains(patch.ResetFields, "skills") {
		return fmt.Errorf("%w: bundled skill membership is immutable", ErrInvalidConfig)
	}
	if !patch.PayloadSet {
		return nil
	}
	return rejectImmutableSkillPayload(patch.Payload)
}

func (s *Service) transitionConfig(ctx context.Context, authority authz.Authority, kind MutationKind, def Definition, before, after *Config) error {
	if s.policy.Transition == nil {
		return fmt.Errorf("%w: backend transition unavailable", ErrInvalidConfig)
	}
	if !s.txBound || s.mutationTx == nil {
		return ErrForbidden
	}
	var beforeClone, afterClone *Config
	if before != nil {
		copy := cloneConfig(*before)
		beforeClone = &copy
	}
	if after != nil {
		copy := cloneConfig(*after)
		afterClone = &copy
	}
	return s.policy.Transition(ctx, s.mutationTx, authority, kind, cloneDefinition(def), beforeClone, afterClone)
}

func patchPayload(current json.RawMessage, patch ConfigPatch) (json.RawMessage, error) {
	if len(current) == 0 && len(patch.ResetFields) != 0 {
		return nil, ErrInvalidConfig
	}
	if !patch.PayloadSet && len(patch.ResetFields) == 0 {
		return cloneRaw(current), nil
	}
	if patch.PayloadSet && len(patch.Payload) == 0 {
		if len(patch.ResetFields) != 0 {
			return nil, ErrInvalidConfig
		}
		return nil, nil
	}
	var owned map[string]json.RawMessage
	if err := json.Unmarshal(nonEmptyJSON(current), &owned); err != nil || owned == nil {
		return nil, ErrInvalidConfig
	}
	var fields map[string]json.RawMessage
	if patch.PayloadSet {
		if err := json.Unmarshal(patch.Payload, &fields); err != nil || fields == nil {
			return nil, ErrInvalidConfig
		}
		maps.Copy(owned, fields)
	}
	for _, key := range patch.ResetFields {
		if key == "" {
			return nil, ErrInvalidConfig
		}
		if _, supplied := fields[key]; supplied {
			return nil, fmt.Errorf("%w: patch and reset overlap", ErrInvalidConfig)
		}
		delete(owned, key)
	}
	return json.Marshal(owned)
}

func mergeObjects(base, overlay json.RawMessage) (json.RawMessage, error) {
	var baseObject, overlayObject map[string]json.RawMessage
	if err := json.Unmarshal(base, &baseObject); err != nil || baseObject == nil {
		return nil, errors.New("definition spec must be an object")
	}
	if err := json.Unmarshal(overlay, &overlayObject); err != nil || overlayObject == nil {
		return nil, errors.New("config payload must be an object")
	}
	for key, value := range overlayObject {
		if string(value) == "null" {
			delete(baseObject, key)
			continue
		}
		baseObject[key] = value
	}
	return json.Marshal(baseObject)
}

// DefinitionPatch changes presentation metadata only; execution identity and
// resource declarations cannot be replaced under existing shared configs.
type DefinitionPatch struct {
	DisplayName *string
	Description *string
}

func (b *Access) managedDefinition(ctx context.Context, id string) (Definition, error) {
	def, err := b.service.getDefinition(ctx, id)
	if err != nil {
		return Definition{}, err
	}
	if def.Source == SourceBuiltin {
		return Definition{}, ErrForbidden
	}
	if !b.authority.IsAdmin() && (def.CreatorUserID == "" || def.CreatorUserID != string(b.authority.UserID())) {
		return Definition{}, ErrNotFound
	}
	return def, nil
}

func (b *Access) UpdateDefinition(ctx context.Context, id string, revision int64, patch DefinitionPatch) (Definition, error) {
	if err := b.ensureActive(); err != nil {
		return Definition{}, err
	}
	if !b.service.txBound {
		var updated Definition
		err := b.service.WithMutationTx(ctx, b.authority, func(mutationCtx context.Context, bound *Access, _ pgx.Tx) error {
			var err error
			updated, err = bound.UpdateDefinition(mutationCtx, id, revision, patch)
			return err
		})
		return updated, err
	}
	def, err := b.managedDefinition(ctx, id)
	if err != nil {
		return Definition{}, err
	}
	if patch.DisplayName != nil {
		def.DisplayName = *patch.DisplayName
	}
	if patch.Description != nil {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(def.Spec, &fields); err != nil {
			return Definition{}, err
		}
		fields["description"], err = json.Marshal(*patch.Description)
		if err != nil {
			return Definition{}, err
		}
		def.Spec, err = json.Marshal(fields)
		if err != nil {
			return Definition{}, err
		}
	}
	if err := validateCustomSpec(def); err != nil {
		return Definition{}, err
	}
	row, err := b.service.q.UpdatePluginDefinitionCAS(ctx, sqlc.UpdatePluginDefinitionCASParams{
		ID: id, Revision: revision, DisplayName: def.DisplayName, Spec: def.Spec,
	})
	if err != nil {
		return Definition{}, mapConflict(err)
	}
	return fromSQLDefinition(row), nil
}

func (b *Access) DeleteDefinition(ctx context.Context, id string, revision int64) error {
	if err := b.ensureActive(); err != nil {
		return err
	}
	if !b.service.txBound {
		return b.service.WithMutationTx(ctx, b.authority, func(mutationCtx context.Context, bound *Access, _ pgx.Tx) error {
			return bound.DeleteDefinition(mutationCtx, id, revision)
		})
	}
	if _, err := b.managedDefinition(ctx, id); err != nil {
		return err
	}
	return b.service.deleteDefinition(ctx, id, revision)
}

func (s *Service) deleteDefinition(ctx context.Context, id string, revision int64) error {
	def, err := s.getDefinition(ctx, id)
	if err != nil {
		return err
	}
	if def.Source == SourceBuiltin {
		return ErrForbidden
	}
	if err := s.q.DeletePluginToolPolicies(ctx, nullableText(id)); err != nil {
		return err
	}
	// The definition FK makes concurrent config creation atomic with deletion.
	// A remaining config aborts this transaction, including policy cleanup.
	deleted, err := s.q.DeletePluginDefinitionCAS(ctx, sqlc.DeletePluginDefinitionCASParams{ID: id, Revision: revision})
	if err != nil {
		return mapConflict(err)
	}
	if deleted != 1 {
		return ErrConflict
	}
	return nil
}
