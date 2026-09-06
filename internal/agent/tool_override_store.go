package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CherryHQ/stella/internal/agent/settingspolicy"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/pkg/db/pgnull"
	sqlc "github.com/CherryHQ/stella/pkg/db/sqlc"
)

// ToolOverrideAbsentVersion is the only version accepted when creating an exact
// override row. It makes a first write conditional without inventing a row to
// version before the user has chosen an override.
const ToolOverrideAbsentVersion = "absent"

// ToolOverrideStore reads persisted tool-visibility overrides for an
// agent+user context. Its Fetch method satisfies ToolOverrideFetcher.
type ToolOverrideStore struct {
	q *sqlc.Queries
}

// NewToolOverrideStore builds a ToolOverrideStore over the given pool.
func NewToolOverrideStore(db *pgxpool.Pool) *ToolOverrideStore {
	return &ToolOverrideStore{q: sqlc.New(db)}
}

// Fetch returns the tool overrides that apply to the given user+agent pair.
func (s *ToolOverrideStore) Fetch(ctx context.Context, userID, agentID string) ([]ToolOverride, error) {
	rows, err := s.q.ListToolOverridesForAgentContext(ctx, sqlc.ListToolOverridesForAgentContextParams{
		UserID: pgnull.Text(userID), AgentID: pgnull.Text(agentID),
	})
	if err != nil {
		return nil, err
	}
	out := make([]ToolOverride, 0, len(rows))
	for _, row := range rows {
		identity, err := persistedToolIdentity(row)
		if err != nil {
			return nil, fmt.Errorf("tool override: invalid persisted identity: %w", err)
		}
		if identity.CoreToolName != "" && isSettingsManagedTool(identity.CoreToolName) {
			continue
		}
		override := ToolOverride{Identity: identity, Scope: row.Scope, Enabled: row.Enabled}
		out = append(out, override)
	}
	return out, nil
}

// ToolOverrideWrite is the durable owner+scope key plus the desired enabled state
// for one tool visibility override.
type ToolOverrideWrite struct {
	Identity ToolIdentity
	Scope    string
	UserID   string
	AgentID  string
	Enabled  bool
}

// ToolOverrideKey identifies one override row for clearing.
type ToolOverrideKey struct {
	Identity ToolIdentity
	Scope    string
	UserID   string
	AgentID  string
}

// ToolOverrideVersion is the safe exact-row projection used by management tools.
type ToolOverrideVersion struct {
	Identity *ToolIdentity `json:"identity,omitempty"`
	ToolName string        `json:"tool_name"`
	Scope    string        `json:"scope"`
	Enabled  bool          `json:"enabled"`
	Version  string        `json:"version"`
	Present  bool          `json:"present"`
	// Family is set only for MCP tools ("mcp:<server>"); generated tools carry
	// their family in the toolmeta registry and the profile tools endpoint.
	Family string `json:"family,omitempty"`
}

// Get returns one exact owner-bound override. Missing rows receive the stable
// absent sentinel so a caller can conditionally create rather than race an
// unguarded upsert.
// ListVersions returns every exact user+agent override in one query. Missing
// tools are represented by callers with ToolOverrideAbsentVersion.
func (s *ToolOverrideStore) ListVersions(ctx context.Context, userID, agentID string) (map[string]ToolOverrideVersion, error) {
	rows, err := s.q.ListToolOverridesForAgentContext(ctx, sqlc.ListToolOverridesForAgentContextParams{
		UserID: pgnull.Text(userID), AgentID: pgnull.Text(agentID),
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]ToolOverrideVersion, len(rows))
	for _, row := range rows {
		identity, err := persistedToolIdentity(row)
		if err != nil {
			return nil, fmt.Errorf("tool override: invalid persisted identity: %w", err)
		}
		if row.Scope != ToolOverrideScopeUserAgent || (identity.CoreToolName != "" && isSettingsManagedTool(identity.CoreToolName)) {
			continue
		}
		out[toolOverrideVersionKey(identity)] = overrideVersion(row)
	}
	return out, nil
}

func (s *ToolOverrideStore) Get(ctx context.Context, k ToolOverrideKey) (ToolOverrideVersion, error) {
	if !isOverrideScope(k.Scope) {
		return ToolOverrideVersion{}, fmt.Errorf("tool override: invalid scope %q", k.Scope)
	}
	identity, err := k.toolIdentity()
	if err != nil {
		return ToolOverrideVersion{}, err
	}
	row, err := s.q.GetToolOverrideByIdentity(ctx, identityParams(identity, k.Scope, k.UserID, k.AgentID))
	if errors.Is(err, pgx.ErrNoRows) {
		version := ToolOverrideVersion{ToolName: identity.CoreToolName, Scope: k.Scope, Version: ToolOverrideAbsentVersion}
		if identity.isPlugin() {
			version.Identity = &identity
		}
		return version, nil
	}
	if err != nil {
		return ToolOverrideVersion{}, err
	}
	return overrideVersion(row), nil
}

// Set upserts a tool visibility override for existing HTTP callers, which keep
// their historical unconditional-write contract.
func (s *ToolOverrideStore) Set(ctx context.Context, w ToolOverrideWrite) error {
	if !isOverrideScope(w.Scope) {
		return fmt.Errorf("tool override: invalid scope %q", w.Scope)
	}
	identity, err := w.toolIdentity()
	if err != nil {
		return err
	}
	if identity.isPlugin() {
		_, err = s.q.UpsertPluginToolOverride(ctx, sqlc.UpsertPluginToolOverrideParams{
			PluginID: pgnull.Text(identity.PluginID), LocalToolName: pgnull.Text(identity.LocalToolName), Scope: w.Scope,
			UserID: pgnull.Text(w.UserID), AgentID: pgnull.Text(w.AgentID), Enabled: w.Enabled,
		})
	} else {
		_, err = s.q.UpsertCoreToolOverride(ctx, sqlc.UpsertCoreToolOverrideParams{
			ToolName: pgnull.Text(identity.CoreToolName), Scope: w.Scope, UserID: pgnull.Text(w.UserID), AgentID: pgnull.Text(w.AgentID), Enabled: w.Enabled,
		})
	}
	return err
}

// SetIfVersion updates an existing row or creates an absent row only when its
// version still matches. A zero-row conditional write is a conflict, never a
// silent retry or an unconditional upsert.
func (s *ToolOverrideStore) SetIfVersion(ctx context.Context, w ToolOverrideWrite, expected string) (ToolOverrideVersion, error) {
	if !isOverrideScope(w.Scope) {
		return ToolOverrideVersion{}, fmt.Errorf("tool override: invalid scope %q", w.Scope)
	}
	identity, err := w.toolIdentity()
	if err != nil {
		return ToolOverrideVersion{}, err
	}
	if expected == ToolOverrideAbsentVersion {
		var row sqlc.ToolOverride
		if identity.isPlugin() {
			row, err = s.q.InsertPluginToolOverrideIfAbsent(ctx, sqlc.InsertPluginToolOverrideIfAbsentParams{
				PluginID: pgnull.Text(identity.PluginID), LocalToolName: pgnull.Text(identity.LocalToolName), Scope: w.Scope,
				UserID: pgnull.Text(w.UserID), AgentID: pgnull.Text(w.AgentID), Enabled: w.Enabled,
			})
		} else {
			row, err = s.q.InsertCoreToolOverrideIfAbsent(ctx, sqlc.InsertCoreToolOverrideIfAbsentParams{
				ToolName: pgnull.Text(identity.CoreToolName), Scope: w.Scope, UserID: pgnull.Text(w.UserID), AgentID: pgnull.Text(w.AgentID), Enabled: w.Enabled,
			})
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return ToolOverrideVersion{}, config.ErrAgentVersionConflict
		}
		if err != nil {
			return ToolOverrideVersion{}, err
		}
		return overrideVersion(row), nil
	}
	expectedAt, err := parseOverrideVersion(expected)
	if err != nil {
		return ToolOverrideVersion{}, config.ErrAgentVersionConflict
	}
	var row sqlc.ToolOverride
	if identity.isPlugin() {
		row, err = s.q.UpdatePluginToolOverrideIfVersion(ctx, sqlc.UpdatePluginToolOverrideIfVersionParams{
			Enabled: w.Enabled, PluginID: pgnull.Text(identity.PluginID), LocalToolName: pgnull.Text(identity.LocalToolName), Scope: w.Scope,
			UserID: pgnull.Text(w.UserID), AgentID: pgnull.Text(w.AgentID), ExpectedUpdatedAt: expectedAt,
		})
	} else {
		row, err = s.q.UpdateCoreToolOverrideIfVersion(ctx, sqlc.UpdateCoreToolOverrideIfVersionParams{
			Enabled: w.Enabled, ToolName: pgnull.Text(identity.CoreToolName), Scope: w.Scope, UserID: pgnull.Text(w.UserID), AgentID: pgnull.Text(w.AgentID), ExpectedUpdatedAt: expectedAt,
		})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ToolOverrideVersion{}, config.ErrAgentVersionConflict
	}
	if err != nil {
		return ToolOverrideVersion{}, err
	}
	return overrideVersion(row), nil
}

// Clear deletes a tool visibility override if present for existing HTTP callers.
func (s *ToolOverrideStore) Clear(ctx context.Context, k ToolOverrideKey) error {
	if !isOverrideScope(k.Scope) {
		return fmt.Errorf("tool override: invalid scope %q", k.Scope)
	}
	identity, err := k.toolIdentity()
	if err != nil {
		return err
	}
	if identity.isPlugin() {
		return s.q.DeletePluginToolOverride(ctx, sqlc.DeletePluginToolOverrideParams{
			PluginID: pgnull.Text(identity.PluginID), LocalToolName: pgnull.Text(identity.LocalToolName), Scope: k.Scope,
			UserID: pgnull.Text(k.UserID), AgentID: pgnull.Text(k.AgentID),
		})
	}
	return s.q.DeleteCoreToolOverride(ctx, sqlc.DeleteCoreToolOverrideParams{
		ToolName: pgnull.Text(identity.CoreToolName), Scope: k.Scope, UserID: pgnull.Text(k.UserID), AgentID: pgnull.Text(k.AgentID),
	})
}

// ClearIfVersion deletes only an existing row at the version returned by Get.
func (s *ToolOverrideStore) ClearIfVersion(ctx context.Context, k ToolOverrideKey, expected string) error {
	if !isOverrideScope(k.Scope) || expected == ToolOverrideAbsentVersion {
		return config.ErrAgentVersionConflict
	}
	identity, err := k.toolIdentity()
	if err != nil {
		return err
	}
	expectedAt, err := parseOverrideVersion(expected)
	if err != nil {
		return config.ErrAgentVersionConflict
	}
	if identity.isPlugin() {
		_, err = s.q.DeletePluginToolOverrideIfVersion(ctx, sqlc.DeletePluginToolOverrideIfVersionParams{
			PluginID: pgnull.Text(identity.PluginID), LocalToolName: pgnull.Text(identity.LocalToolName), Scope: k.Scope,
			UserID: pgnull.Text(k.UserID), AgentID: pgnull.Text(k.AgentID), ExpectedUpdatedAt: expectedAt,
		})
	} else {
		_, err = s.q.DeleteCoreToolOverrideIfVersion(ctx, sqlc.DeleteCoreToolOverrideIfVersionParams{
			ToolName: pgnull.Text(identity.CoreToolName), Scope: k.Scope, UserID: pgnull.Text(k.UserID), AgentID: pgnull.Text(k.AgentID), ExpectedUpdatedAt: expectedAt,
		})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return config.ErrAgentVersionConflict
	}
	return err
}

func identityParams(identity ToolIdentity, scope, userID, agentID string) sqlc.GetToolOverrideByIdentityParams {
	return sqlc.GetToolOverrideByIdentityParams{
		ToolName: pgnull.Text(identity.CoreToolName), PluginID: pgnull.Text(identity.PluginID), LocalToolName: pgnull.Text(identity.LocalToolName),
		Scope: scope, UserID: pgnull.Text(userID), AgentID: pgnull.Text(agentID),
	}
}

func overrideVersion(row sqlc.ToolOverride) ToolOverrideVersion {
	identity, _ := persistedToolIdentity(row)
	toolName := ""
	if row.ToolName.Valid {
		toolName = row.ToolName.String
	}
	version := ToolOverrideVersion{ToolName: toolName, Scope: row.Scope, Enabled: row.Enabled, Present: true, Version: row.UpdatedAt.UTC().Format(time.RFC3339Nano)}
	if identity.isPlugin() {
		version.Identity = &identity
	}
	return version
}

// toolOverrideVersionKey keeps the management projection collision-free while
// retaining the historical core-name key for core tools. Plugin exported names
// are display values and cannot identify a policy row on their own.
func toolOverrideVersionKey(identity ToolIdentity) string {
	if identity.isPlugin() {
		return "plugin:" + identity.PluginID + "\x00" + identity.LocalToolName
	}
	return identity.CoreToolName
}

func (w ToolOverrideWrite) toolIdentity() (ToolIdentity, error) {
	if err := w.Identity.Validate(); err != nil {
		return ToolIdentity{}, fmt.Errorf("tool override: %w", err)
	}
	return w.Identity, nil
}

func (k ToolOverrideKey) toolIdentity() (ToolIdentity, error) {
	if err := k.Identity.Validate(); err != nil {
		return ToolIdentity{}, fmt.Errorf("tool override: %w", err)
	}
	return k.Identity, nil
}

func persistedToolIdentity(row sqlc.ToolOverride) (ToolIdentity, error) {
	if row.PluginID.Valid || row.LocalToolName.Valid {
		identity := ToolIdentity{PluginID: row.PluginID.String, LocalToolName: row.LocalToolName.String}
		if err := identity.Validate(); err != nil {
			return ToolIdentity{}, err
		}
		return identity, nil
	}
	if !row.ToolName.Valid || row.ToolName.String == "" {
		return ToolIdentity{}, fmt.Errorf("core tool_name is required")
	}
	return ToolIdentity{CoreToolName: row.ToolName.String}, nil
}

func parseOverrideVersion(version string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, version)
}

func isSettingsManagedTool(name string) bool {
	_, ok := settingspolicy.Lookup(name)
	return ok
}

func isOverrideScope(scope string) bool {
	switch scope {
	case ToolOverrideScopeSystem, ToolOverrideScopeSystemAgent, ToolOverrideScopeUser, ToolOverrideScopeUserAgent:
		return true
	default:
		return false
	}
}
