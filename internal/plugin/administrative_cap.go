package plugin

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/CherryHQ/stella/pkg/db/sqlc"
)

// AdministrativeCap reads only the published S/SA switch for a shipped plugin.
// Trusted host/background callers must still authorize the durable instance,
// Agent binding and actor. This boolean never grants use of plugin resources.
func (s *Service) AdministrativeCap(ctx context.Context, pluginID, agentID string) (bool, error) {
	if s == nil || s.db == nil || s.catalog == nil {
		return false, ErrForbidden
	}
	def, ok := s.catalog.Get(pluginID)
	if !ok || def.Source != SourceBuiltin {
		return false, ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)
	row, err := q.GetPluginDefinition(ctx, pluginID)
	if err != nil {
		return false, mapNotFound(err)
	}
	persisted := fromSQLDefinition(row)
	if persisted.Source != SourceBuiltin || persisted.Namespace != def.Namespace || persisted.ImplementationKey != def.ImplementationKey || persisted.Backend != def.Backend {
		return false, ErrInvalidDefinition
	}
	var configs []Config
	for _, scope := range []Scope{ScopeSystem, ScopeSystemAgent} {
		ownerAgent := ""
		if scope == ScopeSystemAgent {
			if agentID == "" {
				continue
			}
			ownerAgent = agentID
		}
		rows, err := q.ListPluginConfigsOwned(ctx, sqlc.ListPluginConfigsOwnedParams{PluginID: pluginID, Scope: string(scope), AgentID: nullableText(ownerAgent)})
		if err != nil {
			return false, err
		}
		for _, row := range rows {
			configs = append(configs, fromSQLConfig(row))
		}
	}
	effective, err := Resolve(def, configs, "", agentID)
	if err != nil {
		return false, err
	}
	return effective.IsEffectivelyEnabled, nil
}
