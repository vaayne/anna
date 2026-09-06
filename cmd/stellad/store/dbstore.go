package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	modelcatalog "github.com/CherryHQ/stella/internal/model/catalog"
	"github.com/CherryHQ/stella/internal/platform/config"
	skillpolicy "github.com/CherryHQ/stella/internal/skill/policy"
	"github.com/CherryHQ/stella/pkg/ai"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
	"github.com/CherryHQ/stella/pkg/db/txlock"
)

// DBStore implements config.Store using sqlc queries backed by PostgreSQL.
type DBStore struct {
	q *sqlc.Queries
	// pool is retained (not just wrapped by q) so composite writes that must be
	// atomic — Agent policy mutations and Agent + encrypted Provider credential
	// writes — can open one
	// transaction via Queries.WithTx.
	pool *pgxpool.Pool
}

// ReadAgentSkillPolicy explicitly reads and decodes the policy column. Decode
// failures are surfaced to callers; treating bad bytes as an empty policy would
// make model execution fail open.
func (s *DBStore) ReadAgentSkillPolicy(ctx context.Context, agentID string) (skillpolicy.Policy, error) {
	row, err := s.q.GetAgent(ctx, agentID)
	if err != nil {
		return skillpolicy.Policy{}, fmt.Errorf("read AgentSkillPolicy for %q: %w", agentID, err)
	}
	policy, err := skillpolicy.Decode(row.EnabledBuiltinSkills)
	if err != nil {
		return skillpolicy.Policy{}, fmt.Errorf("read AgentSkillPolicy for %q: %w", agentID, err)
	}
	return policy, nil
}

// SetAgentSkillPolicy serializes a single logical-ref mutation under the Agent
// row lock. The column is the entire concurrency boundary: normal Agent edits
// deliberately never write it, and two different toggles retain each other.
func (s *DBStore) SetAgentSkillPolicy(ctx context.Context, agentID, ref string, enabled bool) (skillpolicy.Policy, error) {
	if err := skillpolicy.ValidateMutationRef(ref); err != nil {
		return skillpolicy.Policy{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return skillpolicy.Policy{}, fmt.Errorf("begin AgentSkillPolicy mutation: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // successful commit makes rollback inert
	qtx := s.q.WithTx(tx)
	raw, err := qtx.GetAgentSkillPolicyForUpdate(ctx, agentID)
	if err != nil {
		return skillpolicy.Policy{}, fmt.Errorf("lock AgentSkillPolicy for %q: %w", agentID, err)
	}
	policy, err := skillpolicy.Decode(raw)
	if err != nil {
		return skillpolicy.Policy{}, fmt.Errorf("decode AgentSkillPolicy for %q: %w", agentID, err)
	}
	next, err := policy.SetEnabled(ref, enabled)
	if err != nil {
		return skillpolicy.Policy{}, err
	}
	bytes, err := next.CanonicalJSON()
	if err != nil {
		return skillpolicy.Policy{}, err
	}
	if err := qtx.UpdateAgentSkillPolicy(ctx, sqlc.UpdateAgentSkillPolicyParams{EnabledBuiltinSkills: bytes, ID: agentID}); err != nil {
		return skillpolicy.Policy{}, fmt.Errorf("write AgentSkillPolicy for %q: %w", agentID, err)
	}
	return commitAgentSkillPolicy(ctx, next, tx.Commit)
}

// commitAgentSkillPolicy isolates PostgreSQL's ambiguous COMMIT boundary for
// deterministic tests. Every Commit-returned error is conservative: return the
// intended policy and make callers reconcile durable database truth.
func commitAgentSkillPolicy(ctx context.Context, next skillpolicy.Policy, commit func(context.Context) error) (skillpolicy.Policy, error) {
	if err := commit(ctx); err != nil {
		return next, fmt.Errorf("%w: %w", skillpolicy.ErrCommitOutcomeUnknown, err)
	}
	return next, nil
}

// NewDBStore creates a new DBStore wrapping the given database connection.
func NewDBStore(db *pgxpool.Pool) *DBStore {
	return &DBStore{q: sqlc.New(db), pool: db}
}

// --- Providers (backed by provider) ---

func (s *DBStore) ListProviders(ctx context.Context) ([]config.Provider, error) {
	rows, err := s.q.ListProviders(ctx)
	if err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	out := make([]config.Provider, len(rows))
	for i, r := range rows {
		out[i] = providerFromDB(r)
	}
	return out, nil
}

// ListProviderIDs returns canonical Provider row IDs without loading Provider
// config, which contains the deployment-global API key.
func (s *DBStore) ListProviderIDs(ctx context.Context) ([]string, error) {
	ids, err := s.q.ListProviderIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list provider ids: %w", err)
	}
	return ids, nil
}

func (s *DBStore) GetProvider(ctx context.Context, id string) (config.Provider, error) {
	snapshot, err := s.GetProviderSnapshot(ctx, id)
	if err != nil {
		return config.Provider{}, err
	}
	return snapshot.Provider, nil
}

// GetProviderSnapshot returns a provider and its conditional-write version from
// the same durable row read. Settings callers must not combine GetProvider with
// a separate version read, or a concurrent write can make that pair incoherent.
func (s *DBStore) GetProviderSnapshot(ctx context.Context, id string) (config.ProviderSnapshot, error) {
	r, err := s.q.GetProvider(ctx, id)
	if err != nil {
		return config.ProviderSnapshot{}, fmt.Errorf("get provider %q: %w", id, err)
	}
	return providerSnapshotFromDB(r), nil
}

// ListProviderSnapshots returns coherent Settings projections. PostgreSQL
// produces each config and updated_at pair from the same row read.
func (s *DBStore) ListProviderSnapshots(ctx context.Context) ([]config.ProviderSnapshot, error) {
	rows, err := s.q.ListProviders(ctx)
	if err != nil {
		return nil, fmt.Errorf("list provider snapshots: %w", err)
	}
	out := make([]config.ProviderSnapshot, len(rows))
	for i, row := range rows {
		out[i] = providerSnapshotFromDB(row)
	}
	return out, nil
}

func (s *DBStore) CreateProvider(ctx context.Context, p config.Provider) error {
	if p.ID == "" {
		p.ID = uuid.Must(uuid.NewV7()).String()
	}
	configJSON, err := json.Marshal(providerConfig(p))
	if err != nil {
		return fmt.Errorf("create provider %q: marshal config: %w", p.ID, err)
	}
	if _, err := s.q.CreateProvider(ctx, sqlc.CreateProviderParams{
		ID:      p.ID,
		Type:    providerType(p),
		Name:    providerName(p),
		Enabled: p.Enabled,
		Config:  configJSON,
	}); err != nil {
		return fmt.Errorf("create provider %q: %w", p.ID, err)
	}
	return nil
}

func (s *DBStore) UpdateProvider(ctx context.Context, p config.Provider) error {
	configJSON, err := json.Marshal(providerConfig(p))
	if err != nil {
		return fmt.Errorf("update provider %q: marshal config: %w", p.ID, err)
	}
	if err := s.q.UpdateProvider(ctx, sqlc.UpdateProviderParams{
		Type:    providerType(p),
		Name:    providerName(p),
		Enabled: p.Enabled,
		Config:  configJSON,
		ID:      p.ID,
	}); err != nil {
		return fmt.Errorf("update provider %q: %w", p.ID, err)
	}
	return nil
}

func (s *DBStore) DeleteProvider(ctx context.Context, id string) error {
	return s.q.DeleteProvider(ctx, id)
}

func (s *DBStore) UpdateProviderIfVersion(ctx context.Context, p config.Provider, version string) (bool, error) {
	updatedAt, err := time.Parse(time.RFC3339Nano, version)
	if err != nil {
		return false, fmt.Errorf("parse provider version: %w", err)
	}
	configJSON, err := json.Marshal(providerConfig(p))
	if err != nil {
		return false, fmt.Errorf("update provider %q: marshal config: %w", p.ID, err)
	}
	rows, err := s.q.UpdateProviderIfVersion(ctx, sqlc.UpdateProviderIfVersionParams{
		Type: providerType(p), Name: providerName(p), Enabled: p.Enabled, Config: configJSON,
		ID: p.ID, ExpectedUpdatedAt: updatedAt,
	})
	if err != nil {
		return false, fmt.Errorf("update provider %q: %w", p.ID, err)
	}
	return rows == 1, nil
}

func (s *DBStore) DeleteProviderIfVersion(ctx context.Context, id, version string) (bool, error) {
	updatedAt, err := time.Parse(time.RFC3339Nano, version)
	if err != nil {
		return false, fmt.Errorf("parse provider version: %w", err)
	}
	rows, err := s.q.DeleteProviderIfVersion(ctx, sqlc.DeleteProviderIfVersionParams{ID: id, ExpectedUpdatedAt: updatedAt})
	if err != nil {
		return false, fmt.Errorf("delete provider %q: %w", id, err)
	}
	return rows == 1, nil
}

// --- Fetched-model cache (backed by provider_models_cache) ---

func (s *DBStore) ListCachedModels(ctx context.Context) ([]config.CachedModel, error) {
	rows, err := s.q.ListProviderModelsCache(ctx)
	if err != nil {
		return nil, fmt.Errorf("list cached models: %w", err)
	}
	var out []config.CachedModel
	for _, r := range rows {
		var modelIDs []string
		if err := json.Unmarshal(r.Models, &modelIDs); err != nil {
			return nil, fmt.Errorf("list cached models: decode %q: %w", r.ProviderID, err)
		}
		for _, id := range modelIDs {
			out = append(out, config.CachedModel{Provider: r.ProviderID, Model: id, SyncedAt: r.UpdatedAt.UTC()})
		}
	}
	return out, nil
}

func (s *DBStore) ReplaceCachedModels(ctx context.Context, providerID string, modelIDs []string) error {
	if modelIDs == nil {
		modelIDs = []string{}
	}
	data, err := json.Marshal(modelIDs)
	if err != nil {
		return fmt.Errorf("replace cached models %q: marshal: %w", providerID, err)
	}
	if err := s.q.UpsertProviderModelsCache(ctx, sqlc.UpsertProviderModelsCacheParams{
		ProviderID: providerID,
		Models:     data,
	}); err != nil {
		return fmt.Errorf("replace cached models %q: %w", providerID, err)
	}
	return nil
}

// GetModelCatalog reads the single models.dev snapshot row. A missing row is
// returned as an error so callers can use the embedded snapshot instead.
func (s *DBStore) GetModelCatalog(ctx context.Context) (modelcatalog.SnapshotRecord, error) {
	row, err := s.q.GetModelCatalog(ctx)
	if err != nil {
		return modelcatalog.SnapshotRecord{}, fmt.Errorf("get model catalog: %w", err)
	}
	return modelcatalog.SnapshotRecord{Payload: row.Payload, ETag: row.Etag, SyncedAt: row.SyncedAt.UTC()}, nil
}

// UpsertModelCatalog stores a synchronized models.dev snapshot.
func (s *DBStore) UpsertModelCatalog(ctx context.Context, record modelcatalog.SnapshotRecord) error {
	if err := s.q.UpsertModelCatalog(ctx, sqlc.UpsertModelCatalogParams{
		Payload: record.Payload, Etag: record.ETag, SyncedAt: record.SyncedAt.UTC(),
	}); err != nil {
		return fmt.Errorf("upsert model catalog: %w", err)
	}
	return nil
}

// --- Agents ---

func (s *DBStore) ListAgents(ctx context.Context) ([]config.Agent, error) {
	rows, err := s.q.ListAgents(ctx)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	out := make([]config.Agent, len(rows))
	for i, r := range rows {
		agent, err := agentFromDB(r)
		if err != nil {
			return nil, fmt.Errorf("list agents: %w", err)
		}
		out[i] = agent
	}
	return out, nil
}

func (s *DBStore) ListEnabledAgents(ctx context.Context) ([]config.Agent, error) {
	rows, err := s.q.ListEnabledAgents(ctx)
	if err != nil {
		return nil, fmt.Errorf("list enabled agents: %w", err)
	}
	out := make([]config.Agent, len(rows))
	for i, r := range rows {
		agent, err := agentFromDB(r)
		if err != nil {
			return nil, fmt.Errorf("list enabled agents: %w", err)
		}
		out[i] = agent
	}
	return out, nil
}

func (s *DBStore) GetAgent(ctx context.Context, id string) (config.Agent, error) {
	snapshot, err := s.GetAgentSnapshot(ctx, id)
	if err != nil {
		return config.Agent{}, err
	}
	return snapshot.Agent, nil
}

// GetAgentSnapshot returns an Agent and the conditional-write version from the
// same durable row read. Agent tools must use this instead of combining an Agent
// read with a later version read, which could otherwise bless stale fields with
// a concurrent UI or admin write's version.
func (s *DBStore) GetAgentSnapshot(ctx context.Context, id string) (config.AgentSnapshot, error) {
	r, err := s.q.GetAgent(ctx, id)
	if err != nil {
		return config.AgentSnapshot{}, fmt.Errorf("get agent %q: %w", id, err)
	}
	snapshot, err := agentSnapshotFromDB(r)
	if err != nil {
		return config.AgentSnapshot{}, fmt.Errorf("get agent %q: %w", id, err)
	}
	return snapshot, nil
}

// ListAgentSnapshots returns coherent Agent Settings projections. Each Agent
// value and its opaque version originate from the same row returned by PostgreSQL.
func (s *DBStore) ListAgentSnapshots(ctx context.Context) ([]config.AgentSnapshot, error) {
	rows, err := s.q.ListAgents(ctx)
	if err != nil {
		return nil, fmt.Errorf("list agent snapshots: %w", err)
	}
	out := make([]config.AgentSnapshot, len(rows))
	for i, row := range rows {
		snapshot, err := agentSnapshotFromDB(row)
		if err != nil {
			return nil, fmt.Errorf("list agent snapshots: %w", err)
		}
		out[i] = snapshot
	}
	return out, nil
}

func (s *DBStore) CreateAgent(ctx context.Context, a config.Agent) error {
	params, err := createAgentParams(a)
	if err != nil {
		return err
	}
	if _, err := s.q.CreateAgent(ctx, params); err != nil {
		return fmt.Errorf("create agent %q: %w", params.ID, err)
	}
	return nil
}

// createAgentParams mints a missing ID, applies the default scope, and validates
// and marshals the sandbox config into insert params. It is shared by the plain
// CreateAgent and the atomic Agent+credentials create so both write identical
// rows.
func createAgentParams(a config.Agent) (sqlc.CreateAgentParams, error) {
	if a.ID == "" {
		a.ID = uuid.Must(uuid.NewV7()).String()
	}
	scope := a.Scope
	if scope == "" {
		scope = config.AgentScopeSystem
	}
	if err := a.Sandbox.Validate(); err != nil {
		return sqlc.CreateAgentParams{}, fmt.Errorf("create agent %q: %w", a.ID, err)
	}
	sandboxJSON, err := marshalSandboxConfig(a.Sandbox)
	if err != nil {
		return sqlc.CreateAgentParams{}, fmt.Errorf("create agent %q: %w", a.ID, err)
	}
	return sqlc.CreateAgentParams{
		ID:                         a.ID,
		Name:                       a.Name,
		Model:                      a.Model,
		ModelThinking:              a.ModelThinking,
		ModelStrong:                a.ModelStrong,
		ModelStrongThinking:        a.ModelStrongThinking,
		ModelFast:                  a.ModelFast,
		ModelFastThinking:          a.ModelFastThinking,
		SystemPrompt:               a.SystemPrompt,
		Soul:                       a.Soul,
		Workspace:                  a.Workspace,
		Sandbox:                    sandboxJSON,
		Scope:                      scope,
		CreatorID:                  a.CreatorID,
		Enabled:                    a.Enabled,
		SystemSettingsToolsEnabled: a.SystemSettingsToolsEnabled,
	}, nil
}

func (s *DBStore) UpdateAgent(ctx context.Context, a config.Agent) error {
	scope := a.Scope
	if scope == "" {
		scope = config.AgentScopeSystem
	}
	if err := a.Sandbox.Validate(); err != nil {
		return fmt.Errorf("update agent %q: %w", a.ID, err)
	}
	sandboxJSON, err := marshalSandboxConfig(a.Sandbox)
	if err != nil {
		return fmt.Errorf("update agent %q: %w", a.ID, err)
	}
	err = s.q.UpdateAgent(ctx, sqlc.UpdateAgentParams{
		ID:                         a.ID,
		Name:                       a.Name,
		Model:                      a.Model,
		ModelThinking:              a.ModelThinking,
		ModelStrong:                a.ModelStrong,
		ModelStrongThinking:        a.ModelStrongThinking,
		ModelFast:                  a.ModelFast,
		ModelFastThinking:          a.ModelFastThinking,
		SystemPrompt:               a.SystemPrompt,
		Soul:                       a.Soul,
		Workspace:                  a.Workspace,
		Sandbox:                    sandboxJSON,
		Scope:                      scope,
		Enabled:                    a.Enabled,
		SystemSettingsToolsEnabled: a.SystemSettingsToolsEnabled,
	})
	if err != nil {
		return fmt.Errorf("update agent %q: %w", a.ID, err)
	}
	return nil
}

// UpdateAgentIfVersion performs the version comparison and mutation in one SQL
// statement. A zero-row update is a normal optimistic-concurrency conflict.
func (s *DBStore) UpdateAgentIfVersion(ctx context.Context, a config.Agent, expectedVersion string) (string, error) {
	params, err := conditionalAgentUpdateParams(a, expectedVersion)
	if err != nil {
		return "", err
	}
	updated, err := s.q.UpdateAgentIfVersion(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", config.ErrAgentVersionConflict
	}
	if err != nil {
		return "", fmt.Errorf("conditional update agent %q: %w", a.ID, err)
	}
	return updated.UTC().Format(time.RFC3339Nano), nil
}

// UpdateAgentIfVersionAndAssignCreator narrows an Agent's scope and grants its
// creator access in one transaction. It shares the assignment relation's
// advisory lock with administrative assignment changes, so a revoke cannot
// commit between the Agent CAS and the insert that would undo it.
func (s *DBStore) UpdateAgentIfVersionAndAssignCreator(ctx context.Context, a config.Agent, expectedVersion, creatorID string) (string, error) {
	params, err := conditionalAgentUpdateParams(a, expectedVersion)
	if err != nil {
		return "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin conditional Agent scope update %q: %w", a.ID, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // successful commit makes rollback inert
	if err := txlock.AdvisoryXactLock(ctx, tx, txlock.AgentAssignmentLockKey(creatorID, a.ID)); err != nil {
		return "", fmt.Errorf("lock creator assignment %q for Agent %q: %w", creatorID, a.ID, err)
	}
	qtx := s.q.WithTx(tx)
	updated, err := qtx.UpdateAgentIfVersion(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", config.ErrAgentVersionConflict
	}
	if err != nil {
		return "", fmt.Errorf("conditional update agent %q: %w", a.ID, err)
	}
	if err := qtx.AssignUserAgent(ctx, sqlc.AssignUserAgentParams{UserID: creatorID, AgentID: a.ID}); err != nil {
		return "", fmt.Errorf("assign creator %q to agent %q: %w", creatorID, a.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit conditional Agent scope update %q: %w", a.ID, err)
	}
	return updated.UTC().Format(time.RFC3339Nano), nil
}

func conditionalAgentUpdateParams(a config.Agent, expectedVersion string) (sqlc.UpdateAgentIfVersionParams, error) {
	expected, err := time.Parse(time.RFC3339Nano, expectedVersion)
	if err != nil {
		return sqlc.UpdateAgentIfVersionParams{}, config.ErrAgentVersionConflict
	}
	scope := a.Scope
	if scope == "" {
		scope = config.AgentScopeSystem
	}
	if err := a.Sandbox.Validate(); err != nil {
		return sqlc.UpdateAgentIfVersionParams{}, fmt.Errorf("update agent %q: %w", a.ID, err)
	}
	sandboxJSON, err := marshalSandboxConfig(a.Sandbox)
	if err != nil {
		return sqlc.UpdateAgentIfVersionParams{}, fmt.Errorf("update agent %q: %w", a.ID, err)
	}
	return sqlc.UpdateAgentIfVersionParams{
		Name: a.Name, Model: a.Model, ModelThinking: a.ModelThinking,
		ModelStrong: a.ModelStrong, ModelStrongThinking: a.ModelStrongThinking,
		ModelFast: a.ModelFast, ModelFastThinking: a.ModelFastThinking,
		SystemPrompt: a.SystemPrompt, Soul: a.Soul, Workspace: a.Workspace,
		Sandbox: sandboxJSON, Scope: scope, Enabled: a.Enabled,
		SystemSettingsToolsEnabled: a.SystemSettingsToolsEnabled,
		ID:                         a.ID, UpdatedAt: expected,
	}, nil
}

func (s *DBStore) DeleteAgent(ctx context.Context, id string) error {
	err := s.q.DeleteAgent(ctx, id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "23001" || pgErr.Code == "23503") &&
		(pgErr.ConstraintName == "webhook_agent_id_fkey" || pgErr.ConstraintName == "library_file_agent_id_fkey") {
		return config.ErrAgentInUse
	}
	return err
}

// --- Channels ---

func (s *DBStore) ListChannels(ctx context.Context) ([]config.Channel, error) {
	rows, err := s.q.ListChannels(ctx)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	out := make([]config.Channel, len(rows))
	for i, r := range rows {
		out[i] = channelFromDB(r)
	}
	return out, nil
}

func (s *DBStore) ListChannelsByType(ctx context.Context, channelType string) ([]config.Channel, error) {
	rows, err := s.q.ListChannelsByType(ctx, channelType)
	if err != nil {
		return nil, fmt.Errorf("list %s channels: %w", channelType, err)
	}
	out := make([]config.Channel, len(rows))
	for i, r := range rows {
		out[i] = channelFromDB(r)
	}
	return out, nil
}

func (s *DBStore) GetChannel(ctx context.Context, id string) (config.Channel, error) {
	r, err := s.q.GetChannel(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return config.Channel{}, config.ErrChannelNotFound
		}
		return config.Channel{}, fmt.Errorf("get channel %q: %w", id, err)
	}
	return channelFromDB(r), nil
}

func (s *DBStore) CreateChannel(ctx context.Context, ch config.Channel) error {
	if ch.ID == "" {
		ch.ID = uuid.Must(uuid.NewV7()).String()
	}
	channelType := effectiveStoredChannelType(ch)
	_, err := s.q.CreateChannel(ctx, sqlc.CreateChannelParams{
		ID:      ch.ID,
		Name:    ch.Name,
		Type:    channelType,
		AgentID: pgtype.Text{String: ch.AgentID, Valid: ch.AgentID != ""},
		Enabled: ch.Enabled,
		Config:  ch.Config,
	})
	return s.channelWriteError(ctx, ch, channelType, err)
}

func (s *DBStore) UpdateChannel(ctx context.Context, ch config.Channel) error {
	channelType := effectiveStoredChannelType(ch)
	_, err := s.q.UpdateChannel(ctx, sqlc.UpdateChannelParams{
		ID:      ch.ID,
		Name:    ch.Name,
		Type:    channelType,
		AgentID: pgtype.Text{String: ch.AgentID, Valid: ch.AgentID != ""},
		Enabled: ch.Enabled,
		Config:  ch.Config,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return config.ErrChannelNotFound
	}
	return s.channelWriteError(ctx, ch, channelType, err)
}

func effectiveStoredChannelType(ch config.Channel) string {
	if ch.Type != "" {
		return ch.Type
	}
	return ch.ID
}

func (s *DBStore) channelWriteError(ctx context.Context, ch config.Channel, channelType string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "channel_pkey" {
		return config.ErrChannelExists
	}
	if !isChannelBindingViolation(err) {
		return err
	}
	rows, listErr := s.q.ListChannels(ctx)
	if listErr != nil {
		return err
	}
	for _, row := range rows {
		existing := channelFromDB(row)
		if existing.ID != ch.ID && existing.AgentID == ch.AgentID && existing.Type == channelType {
			return &config.ChannelBindingConflictError{AgentID: ch.AgentID, Type: channelType, ChannelID: existing.ID}
		}
	}
	return err
}

func isChannelBindingViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "idx_channel_agent_id_type"
}

func (s *DBStore) DeleteChannel(ctx context.Context, id string) error {
	return s.q.DeleteChannel(ctx, id)
}

// --- Plugins ---

func (s *DBStore) ListPlugins(ctx context.Context) ([]config.Plugin, error) {
	return s.mergedPlugins(ctx, nil)
}

func (s *DBStore) ListPluginOverrides(ctx context.Context) ([]config.Plugin, error) {
	rows, err := s.q.ListPluginOverrides(ctx)
	if err != nil {
		return nil, fmt.Errorf("list plugin overrides: %w", err)
	}
	out := make([]config.Plugin, len(rows))
	for i, r := range rows {
		out[i] = pluginFromDB(r)
	}
	return out, nil
}

func (s *DBStore) ListEnabledPlugins(ctx context.Context) ([]config.Plugin, error) {
	filter := func(p config.Plugin) bool { return p.Enabled }
	return s.mergedPlugins(ctx, filter)
}

func (s *DBStore) GetPlugin(ctx context.Context, id string) (config.Plugin, error) {
	builtin, isBuiltin := config.BuiltinPluginByID(id)
	r, dbErr := s.q.GetPlugin(ctx, id)
	if dbErr == nil {
		p := pluginFromDB(r)
		if isBuiltin {
			p.Kind = builtin.Kind
			p.Name = builtin.Name
		}
		return p, nil
	}
	if isBuiltin && errors.Is(dbErr, pgx.ErrNoRows) {
		return config.Plugin{
			ID:      builtin.ID,
			Kind:    builtin.Kind,
			Name:    builtin.Name,
			Enabled: builtin.DefaultEnabled,
			Config:  map[string]any{},
		}, nil
	}
	return config.Plugin{}, fmt.Errorf("get plugin %q: %w", id, dbErr)
}

func (s *DBStore) UpsertPlugin(ctx context.Context, p config.Plugin) error {
	configJSON, err := json.Marshal(p.Config)
	if err != nil {
		return fmt.Errorf("marshal plugin config %q: %w", p.ID, err)
	}
	return s.q.UpsertPlugin(ctx, sqlc.UpsertPluginParams{
		ID:      p.ID,
		Kind:    p.Kind,
		Name:    p.Name,
		Enabled: p.Enabled,
		Config:  configJSON,
	})
}

// SetPluginEnabled and SetPluginConfig each write one column. They are the
// admin kill switch and the channel credential mirror, and those two run
// concurrently: a read-modify-write of the whole row would let either one
// silently restore the other's previous value. The read stays only to name a
// row that may not exist yet.
func (s *DBStore) SetPluginEnabled(ctx context.Context, id string, enabled bool) error {
	p, err := s.GetPlugin(ctx, id)
	if err != nil {
		return fmt.Errorf("set plugin enabled: %w", err)
	}
	return s.q.UpsertPluginEnabled(ctx, sqlc.UpsertPluginEnabledParams{
		ID:      id,
		Kind:    p.Kind,
		Name:    p.Name,
		Enabled: enabled,
	})
}

func (s *DBStore) SetChannelPluginConfig(ctx context.Context, id, kind, name string, cfg map[string]any) error {
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal plugin config %q: %w", id, err)
	}
	return s.q.UpsertPluginConfig(ctx, sqlc.UpsertPluginConfigParams{
		ID:      id,
		Kind:    kind,
		Name:    name,
		Enabled: true,
		Config:  configJSON,
	})
}

func (s *DBStore) SetPluginConfig(ctx context.Context, id string, cfg map[string]any) error {
	p, err := s.GetPlugin(ctx, id)
	if err != nil {
		return fmt.Errorf("set plugin config: %w", err)
	}
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal plugin config %q: %w", id, err)
	}
	return s.q.UpsertPluginConfig(ctx, sqlc.UpsertPluginConfigParams{
		ID:      id,
		Kind:    p.Kind,
		Name:    p.Name,
		Enabled: p.Enabled,
		Config:  configJSON,
	})
}

func (s *DBStore) DeletePlugin(ctx context.Context, id string) error {
	return s.q.DeletePlugin(ctx, id)
}

// --- Manifest plugin overrides ---

func (s *DBStore) GetManifestPluginOverride(ctx context.Context, pluginID string) (config.ManifestPluginOverride, bool, error) {
	row, err := s.q.GetManifestPluginOverride(ctx, pluginID)
	if errors.Is(err, pgx.ErrNoRows) {
		return config.ManifestPluginOverride{}, false, nil
	}
	if err != nil {
		return config.ManifestPluginOverride{}, false, fmt.Errorf("get manifest override %q: %w", pluginID, err)
	}
	return manifestOverrideFromDB(row), true, nil
}

func (s *DBStore) ListManifestPluginOverrides(ctx context.Context) ([]config.ManifestPluginOverride, error) {
	rows, err := s.q.ListManifestPluginOverrides(ctx)
	if err != nil {
		return nil, fmt.Errorf("list manifest overrides: %w", err)
	}
	out := make([]config.ManifestPluginOverride, len(rows))
	for i, r := range rows {
		out[i] = manifestOverrideFromDB(r)
	}
	return out, nil
}

func (s *DBStore) UpsertManifestPluginOverride(ctx context.Context, ov config.ManifestPluginOverride) error {
	var enabled pgtype.Bool
	if ov.Enabled != nil {
		enabled = pgtype.Bool{Bool: *ov.Enabled, Valid: true}
	}
	return s.q.UpsertManifestPluginOverride(ctx, sqlc.UpsertManifestPluginOverrideParams{
		PluginID:           ov.PluginID,
		Enabled:            enabled,
		SessionEnvVaultKey: ov.SessionEnvVaultKey,
		Config:             ov.Config,
	})
}

func (s *DBStore) DeleteManifestPluginOverride(ctx context.Context, pluginID string) error {
	return s.q.DeleteManifestPluginOverride(ctx, pluginID)
}

func manifestOverrideFromDB(r sqlc.PluginOverride) config.ManifestPluginOverride {
	out := config.ManifestPluginOverride{
		PluginID:           r.PluginID,
		SessionEnvVaultKey: r.SessionEnvVaultKey,
		Config:             r.Config,
		UpdatedAt:          r.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if r.Enabled.Valid {
		enabled := r.Enabled.Bool
		out.Enabled = &enabled
	}
	return out
}

// mergedPlugins returns builtins merged with DB overrides, optionally filtered.
func (s *DBStore) mergedPlugins(ctx context.Context, filter func(config.Plugin) bool) ([]config.Plugin, error) {
	rows, err := s.q.ListPluginOverrides(ctx)
	if err != nil {
		return nil, fmt.Errorf("list plugin overrides: %w", err)
	}
	overrides := make(map[string]sqlc.Plugin, len(rows))
	for _, r := range rows {
		overrides[r.ID] = r
	}

	var out []config.Plugin
	seen := make(map[string]bool)

	for _, b := range config.BuiltinPlugins() {
		p := config.Plugin{
			ID:      b.ID,
			Kind:    b.Kind,
			Name:    b.Name,
			Enabled: b.DefaultEnabled,
			Config:  map[string]any{},
		}
		if ov, ok := overrides[b.ID]; ok {
			p.Enabled = ov.Enabled
			var cfg map[string]any
			if len(ov.Config) > 0 && string(ov.Config) != "{}" {
				_ = json.Unmarshal(ov.Config, &cfg)
			}
			if cfg != nil {
				p.Config = cfg
			}
		}
		if filter == nil || filter(p) {
			out = append(out, p)
		}
		seen[b.ID] = true
	}

	for _, r := range rows {
		if seen[r.ID] {
			continue
		}
		p := pluginFromDB(r)
		if filter == nil || filter(p) {
			out = append(out, p)
		}
	}

	return out, nil
}

// --- Chat Agents ---

func (s *DBStore) GetChatAgent(ctx context.Context, channelID, platform, chatID string) (string, error) {
	if channelID == "" {
		channelID = platform
	}
	r, err := s.q.GetChatAgent(ctx, sqlc.GetChatAgentParams{
		ChannelID: channelID,
		Platform:  platform,
		ChatID:    chatID,
	})
	if err != nil {
		return "", fmt.Errorf("get chat agent: %w", err)
	}
	return r.AgentID, nil
}

func (s *DBStore) SetChatAgent(ctx context.Context, channelID, platform, chatID, agentID string) error {
	if channelID == "" {
		channelID = platform
	}
	return s.q.UpsertChatAgent(ctx, sqlc.UpsertChatAgentParams{
		ChannelID: channelID,
		Platform:  platform,
		ChatID:    chatID,
		AgentID:   agentID,
	})
}

func (s *DBStore) DeleteChatAgent(ctx context.Context, channelID, platform, chatID string) error {
	if channelID == "" {
		channelID = platform
	}
	return s.q.DeleteChatAgent(ctx, sqlc.DeleteChatAgentParams{
		ChannelID: channelID,
		Platform:  platform,
		ChatID:    chatID,
	})
}

// --- Settings ---

func (s *DBStore) GetSetting(ctx context.Context, key string) (string, error) {
	r, err := s.q.GetSetting(ctx, key)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("get setting %q: %w", key, err)
	}
	return r.Value, nil
}

func (s *DBStore) SetSetting(ctx context.Context, key, value string) error {
	return s.q.UpsertSetting(ctx, sqlc.UpsertSettingParams{
		Key:   key,
		Value: value,
	})
}

// SetSettingIfValue is the narrow compare-and-set port for Settings tools.
// Other transports retain their existing unconditional SetSetting contract.
func (s *DBStore) SetSettingIfValue(ctx context.Context, key, expectedValue, value string) (bool, error) {
	rows, err := s.q.UpsertSettingIfValue(ctx, sqlc.UpsertSettingIfValueParams{Key: key, ExpectedValue: expectedValue, Value: value})
	if err != nil {
		return false, fmt.Errorf("set setting %q if unchanged: %w", key, err)
	}
	return rows == 1, nil
}

// --- Snapshot ---

func (s *DBStore) Snapshot(ctx context.Context, agentID string) (*config.Snapshot, error) {
	ag, err := s.q.GetAgent(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("snapshot: get agent %q: %w", agentID, err)
	}
	policy, err := skillpolicy.Decode(ag.EnabledBuiltinSkills)
	if err != nil {
		return nil, fmt.Errorf("snapshot: decode AgentSkillPolicy for %q: %w", agentID, err)
	}

	plugins, err := s.mergedPlugins(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("snapshot: list plugins: %w", err)
	}

	// Model roles are deployment-wide defaults that an agent overrides field by
	// field; the vision role has no per-agent form at all. Merging here means
	// every downstream reader keeps seeing one already-resolved set of tiers.
	defaults, err := config.LoadDefaultModels(ctx, s)
	if err != nil {
		return nil, fmt.Errorf("snapshot: load default models: %w", err)
	}
	models := config.MergeAgentModels(defaults, config.Agent{
		Model:               ag.Model,
		ModelThinking:       ag.ModelThinking,
		ModelStrong:         ag.ModelStrong,
		ModelStrongThinking: ag.ModelStrongThinking,
		ModelFast:           ag.ModelFast,
		ModelFastThinking:   ag.ModelFastThinking,
	})

	providers, modelInputs, modelCosts, defaultCreds, err := s.resolveProviders(ctx, models.Model, models.ModelStrong, models.ModelFast, defaults.ModelVision)
	if err != nil {
		return nil, err
	}

	defaultProvID, _ := config.ParseModelRef(models.Model)

	sandboxCfg, err := parseSandboxConfig(ag.Sandbox)
	if err != nil {
		return nil, fmt.Errorf("snapshot: parse agent sandbox config %q: %w", agentID, err)
	}

	snap := &config.Snapshot{
		AgentID:             agentID,
		Provider:            defaultProvID,
		Model:               models.Model,
		ModelThinking:       models.ModelThinking,
		ModelStrong:         models.ModelStrong,
		ModelStrongThinking: models.ModelStrongThinking,
		ModelFast:           models.ModelFast,
		ModelFastThinking:   models.ModelFastThinking,
		ModelVision:         defaults.ModelVision,
		Workspace:           ag.Workspace,
		Sandbox:             sandboxCfg,
		APIKey:              defaultCreds.APIKey,
		BaseURL:             defaultCreds.BaseURL,
		SystemPrompt:        ag.SystemPrompt,
		Soul:                ag.Soul,
		Providers:           providers,
		ModelInputs:         modelInputs,
		ModelCosts:          modelCosts,
		DisabledSkillRefs:   append([]string(nil), policy.Disabled...),
		Plugins:             plugins,
	}

	if val, err := s.GetSetting(ctx, "runner"); err == nil && val != "" {
		_ = json.Unmarshal([]byte(val), &snap.Runner)
	}
	if val, err := s.GetSetting(ctx, "compaction"); err == nil && val != "" {
		_ = json.Unmarshal([]byte(val), &snap.Compaction)
	}
	if val, err := s.GetSetting(ctx, "scheduler"); err == nil && val != "" {
		_ = json.Unmarshal([]byte(val), &snap.Scheduler)
	}
	if snap.Runner.IdleTimeout == 0 {
		snap.Runner.IdleTimeout = 10
	}

	return snap, nil
}

// resolveProviders returns the credentials for every provider referenced by the
// given model refs, the declared input modalities of those providers' models,
// and the credentials of the first ref's provider.
func (s *DBStore) resolveProviders(ctx context.Context, models ...string) (map[string]config.ProviderCreds, map[config.ModelKey][]string, map[config.ModelKey]ai.ModelCost, config.ProviderCreds, error) {
	provIDs := collectProviderIDs(models...)
	rows, err := s.q.ListProviders(ctx)
	if err != nil {
		return nil, nil, nil, config.ProviderCreds{}, fmt.Errorf("snapshot: list providers: %w", err)
	}

	provs := make([]config.Provider, 0, len(rows))
	for _, row := range rows {
		provs = append(provs, providerFromDB(row))
	}
	// One index for every model role — agent tiers, vision, embedding — so a
	// reference resolves the same way whoever asks. Type aliases live in there.
	index := config.NewProviderIndex(provs)
	catalog, _, catalogErr := modelcatalog.Load(ctx, s, nil)
	if catalogErr != nil {
		return nil, nil, nil, config.ProviderCreds{}, catalogErr
	}
	cached, cacheErr := s.ListCachedModels(ctx)
	if cacheErr != nil {
		return nil, nil, nil, config.ProviderCreds{}, fmt.Errorf("snapshot: list cached models: %w", cacheErr)
	}
	fetchedByProvider := make(map[string]map[string]bool)
	for _, model := range cached {
		if fetchedByProvider[model.Provider] == nil {
			fetchedByProvider[model.Provider] = map[string]bool{}
		}
		fetchedByProvider[model.Provider][model.Model] = true
	}

	creds := make(map[string]config.ProviderCreds, len(provIDs))
	modelInputs := make(map[config.ModelKey][]string)
	modelCosts := make(map[config.ModelKey]ai.ModelCost)
	for _, pid := range provIDs {
		p, ok := index.Lookup(pid)
		if !ok {
			continue
		}
		creds[pid] = config.ProviderCreds{Type: p.Type, APIKey: p.APIKey, BaseURL: p.BaseURL, ProviderID: p.ID}
		modelIDs := map[string]bool{}
		for _, ref := range models {
			refProvider, modelID := config.ParseModelRef(ref)
			if refProvider == pid && modelID != "" {
				modelIDs[modelID] = true
			}
		}
		for modelID := range p.Models {
			modelIDs[modelID] = true
		}
		for modelID := range modelIDs {
			resolved := modelcatalog.Resolve(p, modelID, fetchedByProvider[p.ID][modelID], catalog)
			if !resolved.Found {
				continue
			}
			key := config.ModelKey{Provider: pid, Model: modelID}
			if resolved.Model.Input != nil {
				modelInputs[key] = append([]string(nil), resolved.Model.Input...)
			}
			if providerCostPresent(resolved.Model.Cost) {
				modelCosts[key] = modelcatalog.RuntimeCost(resolved.Model.Cost)
			}
		}
	}

	var defaultModel string
	if len(models) > 0 {
		defaultModel = models[0]
	}
	defaultProvID, _ := config.ParseModelRef(defaultModel)
	defaultCreds := creds[defaultProvID]

	return creds, modelInputs, modelCosts, defaultCreds, nil
}

// --- Bootstrap ---

const defaultStellaSoul = `You are Stella — a sharp, efficient personal AI assistant.

- Warm but not chatty. Friendly but not performative.
- Lead with answers, not preamble.
- Match the user's energy: casual when they're casual, precise when they need precision.
- Own your mistakes quickly. No hedging or over-apologizing.
- Use humor sparingly and naturally — never forced.`

// DefaultStellaAgentID is the durable ID reserved for Stella's built-in Agent.
// Runtime policy and production smoke tests share this one identity boundary.
const DefaultStellaAgentID = "stella"

// Seed removes legacy configuration and creates Stella only for an empty agent catalog.
func (s *DBStore) Seed(ctx context.Context) error {
	// The trace hook is no longer a plugin; drop any stale row from prior versions.
	if err := s.DeletePlugin(ctx, "hook/trace"); err != nil {
		return fmt.Errorf("seed: delete stale trace plugin: %w", err)
	}

	agents, err := s.q.ListAgents(ctx)
	if err != nil {
		return fmt.Errorf("seed: list agents: %w", err)
	}
	if len(agents) > 0 {
		return nil
	}
	workspace := filepath.Join(config.StellaHome(), "agents", DefaultStellaAgentID)
	sandboxJSON, err := marshalSandboxConfig(config.SandboxConfig{})
	if err != nil {
		return fmt.Errorf("seed: marshal stella sandbox config: %w", err)
	}
	if err := s.q.SeedAgent(ctx, sqlc.SeedAgentParams{
		ID:                         DefaultStellaAgentID,
		Name:                       "Stella",
		SystemPrompt:               defaultStellaSoul,
		Workspace:                  workspace,
		Sandbox:                    sandboxJSON,
		Scope:                      config.AgentScopeSystem,
		Enabled:                    true,
		SystemSettingsToolsEnabled: true,
	}); err != nil {
		return fmt.Errorf("seed: create stella agent: %w", err)
	}

	return nil
}

// --- Helpers ---

func marshalSandboxConfig(cfg config.SandboxConfig) (json.RawMessage, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal sandbox config: %w", err)
	}
	return data, nil
}

func parseSandboxConfig(raw json.RawMessage) (config.SandboxConfig, error) {
	var cfg config.SandboxConfig
	if len(raw) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return config.SandboxConfig{}, fmt.Errorf("parse sandbox config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return config.SandboxConfig{}, err
	}
	return cfg, nil
}

func providerSnapshotFromDB(r sqlc.Provider) config.ProviderSnapshot {
	return config.ProviderSnapshot{
		Provider: providerFromDB(r),
		Version:  r.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func providerFromDB(r sqlc.Provider) config.Provider {
	cfg := map[string]any{}
	if len(r.Config) > 0 {
		_ = json.Unmarshal(r.Config, &cfg)
	}
	apiKey, _ := cfg["api_key"].(string)
	baseURL, _ := cfg["base_url"].(string)
	return config.Provider{
		ID:          r.ID,
		Type:        r.Type,
		Name:        providerDisplayName(r.Name, r.ID),
		Enabled:     r.Enabled,
		APIKey:      apiKey,
		BaseURL:     baseURL,
		Models:      providerModelsFromAny(cfg["models"]),
		CatalogID:   stringValue(cfg["catalog_id"]),
		ModelPolicy: stringValue(cfg["model_policy"]),
	}
}

type providerConfigPayload struct {
	APIKey      string                                  `json:"api_key"`
	BaseURL     string                                  `json:"base_url"`
	Models      map[string]config.ProviderModelOverride `json:"models,omitempty"`
	CatalogID   string                                  `json:"catalog_id,omitempty"`
	ModelPolicy string                                  `json:"model_policy,omitempty"`
}

func providerConfig(p config.Provider) providerConfigPayload {
	return providerConfigPayload{
		APIKey:      p.APIKey,
		BaseURL:     p.BaseURL,
		Models:      normalizeProviderModels(p.Models),
		CatalogID:   p.CatalogID,
		ModelPolicy: p.ModelPolicy,
	}
}

func providerType(p config.Provider) string {
	if p.Type != "" {
		return p.Type
	}
	return p.ID
}

func providerName(p config.Provider) string {
	if p.Name != "" {
		return p.Name
	}
	return p.ID
}

func providerDisplayName(name, fallback string) string {
	if name != "" {
		return name
	}
	return fallback
}

func normalizeProviderModels(models map[string]config.ProviderModelOverride) map[string]config.ProviderModelOverride {
	if len(models) == 0 {
		return nil
	}
	out := make(map[string]config.ProviderModelOverride, len(models))
	maps.Copy(out, models)
	return out
}

func providerModelsFromAny(value any) map[string]config.ProviderModelOverride {
	if value == nil {
		return nil
	}
	rawModels, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	models := make(map[string]config.ProviderModelOverride, len(rawModels))
	for id, raw := range rawModels {
		data, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		var model config.ProviderModelOverride
		if err := json.Unmarshal(data, &model); err != nil {
			continue
		}
		// Missing enabled is presence, not true. The effective resolver applies
		// the provider policy later.
		models[id] = model
	}
	return models
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func providerCostPresent(c config.ProviderModelCost) bool {
	return c.Input != nil || c.Output != nil || c.CacheRead != nil || c.CacheWrite != nil || c.Reasoning != nil || c.InputAudio != nil || c.OutputAudio != nil || len(c.Tiers) > 0
}

func agentSnapshotFromDB(r sqlc.Agent) (config.AgentSnapshot, error) {
	agent, err := agentFromDB(r)
	if err != nil {
		return config.AgentSnapshot{}, err
	}
	return config.AgentSnapshot{Agent: agent, Version: r.UpdatedAt.UTC().Format(time.RFC3339Nano)}, nil
}

func agentFromDB(r sqlc.Agent) (config.Agent, error) {
	scope := r.Scope
	if scope == "" {
		scope = config.AgentScopeSystem
	}
	sandboxCfg, err := parseSandboxConfig(r.Sandbox)
	if err != nil {
		return config.Agent{}, fmt.Errorf("parse agent %q sandbox config: %w", r.ID, err)
	}
	return config.Agent{
		ID:                         r.ID,
		Name:                       r.Name,
		Model:                      r.Model,
		ModelThinking:              r.ModelThinking,
		ModelStrong:                r.ModelStrong,
		ModelStrongThinking:        r.ModelStrongThinking,
		ModelFast:                  r.ModelFast,
		ModelFastThinking:          r.ModelFastThinking,
		SystemPrompt:               r.SystemPrompt,
		Soul:                       r.Soul,
		Workspace:                  r.Workspace,
		Sandbox:                    sandboxCfg,
		Scope:                      scope,
		CreatorID:                  r.CreatorID,
		Enabled:                    r.Enabled,
		SystemSettingsToolsEnabled: r.SystemSettingsToolsEnabled,
	}, nil
}

func collectProviderIDs(models ...string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, m := range models {
		if m == "" {
			continue
		}
		pid, _ := config.ParseModelRef(m)
		if pid != "" && !seen[pid] {
			seen[pid] = true
			out = append(out, pid)
		}
	}
	return out
}

func pluginFromDB(r sqlc.Plugin) config.Plugin {
	var cfg map[string]any
	if len(r.Config) > 0 && string(r.Config) != "{}" {
		_ = json.Unmarshal(r.Config, &cfg)
	}
	if cfg == nil {
		cfg = make(map[string]any)
	}
	return config.Plugin{
		ID:      r.ID,
		Kind:    r.Kind,
		Name:    r.Name,
		Enabled: r.Enabled,
		Config:  cfg,
	}
}

func channelFromDB(r sqlc.Channel) config.Channel {
	agentID := ""
	if r.AgentID.Valid {
		agentID = r.AgentID.String
	}
	channelType := r.Type
	if channelType == "" {
		channelType = r.ID
	}
	return config.Channel{
		ID:      r.ID,
		Name:    r.Name,
		Type:    channelType,
		AgentID: agentID,
		Enabled: r.Enabled,
		Config:  r.Config,
	}
}
