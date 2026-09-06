package plugin

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
)

// ResolvedPlugin is the private, runtime-facing result for one plugin. Config
// contains persisted credentials references and therefore is never returned by
// value without a defensive clone.
type ResolvedPlugin struct {
	Definition Definition
	Config     *Config
	Effective  Effective
}

// Snapshot is an immutable view of visible plugin definitions and context
// configuration for one trusted authority. ID and namespace resolution remain
// separate: a namespace winner must not hide another definition's ID lookup.
type Snapshot struct {
	catalog   *Catalog
	configs   []Config
	userID    string
	agentID   string
	authority authz.Authority
}

// Authority returns the trusted authority that produced this snapshot. The
// value is immutable and is the only owner identity a downstream provider may
// use for snapshot-scoped operations.
func (s Snapshot) Authority() authz.Authority { return s.authority }

// Get returns a defensive copy of the selected plugin.
func (s Snapshot) Get(pluginID string) (ResolvedPlugin, bool) {
	def, ok := s.catalog.Get(pluginID)
	if !ok {
		return ResolvedPlugin{}, false
	}
	effective, err := Resolve(def, configsFor(def, s.configs), s.userID, s.agentID)
	if err != nil {
		return ResolvedPlugin{}, false
	}
	return s.resolvedPlugin(def, effective), true
}

// Resolve returns one selected effective configuration from the snapshot.
func (s Snapshot) Resolve(pluginID string) (Effective, error) {
	def, ok := s.catalog.Get(pluginID)
	if !ok {
		return Effective{}, ErrNotFound
	}
	effective, err := Resolve(def, configsFor(def, s.configs), s.userID, s.agentID)
	if err != nil {
		return Effective{}, err
	}
	return cloneEffective(effective), nil
}

// ResolveNamespace selects the namespace winner from the snapshot's same
// context. A disabled or invalid winner is returned as such and cannot fall
// back to another definition.
func (s Snapshot) ResolveNamespace(namespace string) (Effective, error) {
	if s.catalog == nil {
		return Effective{}, fmt.Errorf("%w: namespace %q is not configured", ErrNotFound, namespace)
	}
	effective, err := ResolveNamespace(s.catalog, s.configs, namespace, s.userID, s.agentID)
	if err != nil {
		return Effective{}, err
	}
	return cloneEffective(effective), nil
}

// Definitions returns defensive copies of all definitions visible in this
// snapshot, in deterministic namespace and ID order.
func (s Snapshot) Definitions() []Definition {
	if s.catalog == nil {
		return nil
	}
	return s.catalog.Definitions()
}

// ResolveSnapshot reads definitions and relevant configuration rows in one
// repeatable-read, read-only transaction. Agent identities are validated by
// the central Agent PEP before the database snapshot begins.
func (s *Service) ResolveSnapshot(ctx context.Context, authority authz.Authority, agentID string) (Snapshot, error) {
	if s == nil || s.db == nil || s.catalog == nil || !authority.Valid() {
		return Snapshot{}, ErrForbidden
	}
	userID, resolvedAgentID, empty, err := s.snapshotIdentity(ctx, authority, agentID)
	if err != nil {
		return Snapshot{}, err
	}
	if empty {
		return Snapshot{authority: authority}, nil
	}

	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Snapshot{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)
	rows, err := q.ListPluginDefinitions(ctx)
	if err != nil {
		return Snapshot{}, err
	}

	defs := make([]Definition, 0, len(rows))
	for _, row := range rows {
		def := fromSQLDefinition(row)
		if def.Source == SourceBuiltin {
			if _, ok := s.catalog.Get(def.ID); !ok {
				continue // A missing shipped definition is dormant.
			}
		}
		if err := def.Validate(); err != nil {
			return Snapshot{}, err
		}

		defs = append(defs, def)
	}
	configRows, err := q.ListPluginConfigsForContext(ctx, sqlc.ListPluginConfigsForContextParams{
		AgentID: nullableText(resolvedAgentID),
		UserID:  nullableText(userID),
	})
	if err != nil {
		return Snapshot{}, err
	}
	knownDefs := make(map[string]Definition, len(defs))
	configsByID := make(map[string][]Config, len(defs))
	for _, def := range defs {
		knownDefs[def.ID] = def
	}
	for _, row := range configRows {
		def, ok := knownDefs[row.PluginID]
		if !ok {
			continue // Configs for dormant definitions are not part of a snapshot.
		}
		config := cloneConfig(fromSQLConfig(row))
		if config.PluginID != def.ID || config.Namespace != def.Namespace || !matchesContext(config, userID, resolvedAgentID) {
			return Snapshot{}, fmt.Errorf("%w: definition or owner mismatch", ErrInvalidConfig)
		}
		if err := config.Validate(); err != nil {
			return Snapshot{}, err
		}
		configsByID[def.ID] = append(configsByID[def.ID], config)
	}

	visibleDefs := make([]Definition, 0, len(defs))
	for _, def := range defs {
		if def.Source == SourceCustom && !customVisible(configsByID[def.ID], userID, resolvedAgentID) {
			continue // Shared negative records do not grant custom discovery.
		}
		visibleDefs = append(visibleDefs, def)
	}

	// ResolveNamespace needs one catalog containing both shipped and custom
	// definitions. It still chooses by payload-bearing owner before Resolve,
	// preserving winner-first semantics for same-namespace definitions.
	contextCatalog := NewCatalog()
	for _, def := range visibleDefs {
		if err := contextCatalog.Register(def); err != nil {
			return Snapshot{}, err
		}
	}

	allConfigs := make([]Config, 0)
	for _, def := range visibleDefs {
		configs := configsByID[def.ID]
		allConfigs = append(allConfigs, configs...)
	}
	return Snapshot{catalog: contextCatalog, configs: allConfigs, userID: userID, agentID: resolvedAgentID, authority: authority}, nil
}

func (s Snapshot) resolvedPlugin(def Definition, effective Effective) ResolvedPlugin {
	var selected *Config
	for _, config := range configsFor(def, s.configs) {
		if config.ID == effective.ConfigID {
			copy := cloneConfig(config)
			selected = &copy
			break
		}
	}
	return ResolvedPlugin{Definition: cloneDefinition(def), Config: selected, Effective: cloneEffective(effective)}
}

func (s *Service) snapshotIdentity(ctx context.Context, authority authz.Authority, agentID string) (userID, resolvedAgentID string, empty bool, err error) {
	switch authority.Kind() {
	case authz.ActorUser:
		userID = string(authority.UserID())
		if agentID == "" {
			return userID, "", false, nil
		}
		if s.agents == nil {
			return "", "", false, ErrForbidden
		}
		if err := s.agents.Authorize(ctx, authority, agentID, authz.ActionExecute); err != nil {
			return "", "", false, err
		}
		return userID, agentID, false, nil
	case authz.ActorAgent, authz.ActorGroupAgent:
		trustedAgentID := string(authority.AgentID())
		if agentID != "" && agentID != trustedAgentID {
			return "", "", false, ErrForbidden
		}
		if s.agents == nil {
			return "", "", false, ErrForbidden
		}
		if err := s.agents.Authorize(ctx, authority, trustedAgentID, authz.ActionExecute); err != nil {
			return "", "", false, err
		}
		return string(authority.UserID()), trustedAgentID, false, nil
	case authz.ActorGuest:
		if agentID != "" {
			return "", "", false, ErrForbidden
		}
		return "", "", true, nil
	case authz.ActorSystem:
		if agentID != "" {
			return "", "", false, ErrForbidden
		}
		return "", "", false, nil
	default:
		return "", "", false, ErrForbidden
	}
}

func customVisible(configs []Config, userID, agentID string) bool {
	for _, config := range configs {
		switch config.Scope {
		case ScopeSystem:
			if config.UserID == "" && config.AgentID == "" && len(config.Payload) != 0 {
				return true
			}
		case ScopeSystemAgent:
			if config.UserID == "" && config.AgentID == agentID && agentID != "" && len(config.Payload) != 0 {
				return true
			}
		case ScopeUser:
			if config.UserID == userID && config.AgentID == "" && userID != "" {
				return true
			}
		case ScopeUserAgent:
			if config.UserID == userID && config.AgentID == agentID && userID != "" && agentID != "" {
				return true
			}
		}
	}
	return false
}

func cloneEffective(effective Effective) Effective {
	effective.Payload = cloneRaw(effective.Payload)
	return effective
}
