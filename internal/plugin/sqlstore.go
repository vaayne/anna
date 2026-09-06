package plugin

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/CherryHQ/stella/pkg/db/sqlc"
)

func (s *Service) getDefinition(ctx context.Context, id string) (Definition, error) {
	row, err := s.q.GetPluginDefinition(ctx, id)
	if err != nil {
		return Definition{}, mapNotFound(err)
	}
	def := fromSQLDefinition(row)
	if def.Source == SourceBuiltin {
		if _, exists := s.catalog.Get(def.ID); !exists {
			return Definition{}, ErrNotFound
		}
	}
	if err := def.Validate(); err != nil {
		return Definition{}, err
	}
	return def, nil
}

func (s *Service) listDefinitions(ctx context.Context) ([]Definition, error) {
	rows, err := s.q.ListPluginDefinitions(ctx)
	if err != nil {
		return nil, err
	}
	defs := make([]Definition, 0, len(rows))
	for _, row := range rows {
		defs = append(defs, fromSQLDefinition(row))
	}
	return defs, nil
}

func (s *Service) listConfigs(ctx context.Context, pluginID string, scope Scope, userID, agentID string) ([]Config, error) {
	rows, err := s.q.ListPluginConfigsOwned(ctx, sqlc.ListPluginConfigsOwnedParams{
		PluginID: pluginID, Scope: string(scope), UserID: nullableText(userID), AgentID: nullableText(agentID),
	})
	if err != nil {
		return nil, err
	}
	configs := make([]Config, 0, len(rows))
	for _, row := range rows {
		configs = append(configs, fromSQLConfig(row))
	}
	return configs, nil
}

func (s *Service) createConfig(ctx context.Context, config Config) (Config, error) {
	row, err := s.q.CreatePluginConfig(ctx, sqlc.CreatePluginConfigParams{
		ID: config.ID, PluginID: config.PluginID, Namespace: config.Namespace, Scope: string(config.Scope),
		UserID: nullableText(config.UserID), AgentID: nullableText(config.AgentID), Enabled: nullableBool(config.Enabled),
		Config: config.Payload, CredentialRefs: nonEmptyJSON(config.CredentialRefs), Revision: config.Revision,
	})
	if err != nil {
		return Config{}, mapConflict(err)
	}
	return fromSQLConfig(row), nil
}

func (s *Service) updateConfigCAS(ctx context.Context, id string, revision int64, enabled *bool, payload, refs json.RawMessage) (Config, error) {
	row, err := s.q.UpdatePluginConfigCAS(ctx, sqlc.UpdatePluginConfigCASParams{
		ID: id, Revision: revision, Enabled: nullableBool(enabled), Config: payload, CredentialRefs: nonEmptyJSON(refs),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Config{}, ErrConflict
		}
		return Config{}, mapConflict(err)
	}
	return fromSQLConfig(row), nil
}

func (s *Service) moveConfigCAS(ctx context.Context, id string, revision int64, scope Scope, userID, agentID string, enabled *bool, payload, refs json.RawMessage) (Config, error) {
	row, err := s.q.MovePluginConfigCAS(ctx, sqlc.MovePluginConfigCASParams{
		ID: id, Scope: string(scope), UserID: nullableText(userID), AgentID: nullableText(agentID),
		Enabled: nullableBool(enabled), Config: payload, CredentialRefs: nonEmptyJSON(refs), Revision: revision,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Config{}, ErrConflict
		}
		return Config{}, mapConflict(err)
	}
	return fromSQLConfig(row), nil
}

func (s *Service) deleteConfigCAS(ctx context.Context, id string, revision int64, pluginID string) (bool, error) {
	rows, err := s.q.DeletePluginConfigCAS(ctx, sqlc.DeletePluginConfigCASParams{ID: id, Revision: revision, PluginID: pluginID})
	return rows == 1, err
}

func (s *Service) resetBuiltinConfig(ctx context.Context, id string, revision int64, pluginID string) (Config, error) {
	row, err := s.q.ResetBuiltinPluginConfig(ctx, sqlc.ResetBuiltinPluginConfigParams{ID: id, Revision: revision, PluginID: pluginID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Config{}, ErrConflict
		}
		return Config{}, mapConflict(err)
	}
	return fromSQLConfig(row), nil
}

func (s *Service) createCustom(ctx context.Context, def Definition, config Config) (Definition, Config, error) {
	defRow, err := s.q.CreatePluginDefinition(ctx, sqlc.CreatePluginDefinitionParams{
		ID: def.ID, Namespace: def.Namespace, DisplayName: def.DisplayName, Backend: string(def.Backend), Source: string(def.Source),
		ImplementationKey: def.ImplementationKey, Spec: def.Spec, DefaultEnabled: def.DefaultEnabled, Revision: def.Revision, CreatorUserID: nullableText(def.CreatorUserID),
	})
	if err != nil {
		return Definition{}, Config{}, err
	}
	configRow, err := s.q.CreatePluginConfig(ctx, sqlc.CreatePluginConfigParams{
		ID: config.ID, PluginID: config.PluginID, Namespace: config.Namespace, Scope: string(config.Scope), UserID: nullableText(config.UserID), AgentID: nullableText(config.AgentID),
		Enabled: nullableBool(config.Enabled), Config: config.Payload, CredentialRefs: nonEmptyJSON(config.CredentialRefs), Revision: config.Revision,
	})
	if err != nil {
		return Definition{}, Config{}, mapConflict(err)
	}
	return fromSQLDefinition(defRow), fromSQLConfig(configRow), nil
}

func nullableText(value string) pgtype.Text { return pgtype.Text{String: value, Valid: value != ""} }

func nullableBool(value *bool) pgtype.Bool {
	if value == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *value, Valid: true}
}

func boolValue(value pgtype.Bool) *bool {
	if !value.Valid {
		return nil
	}
	result := value.Bool
	return &result
}

func textValue(value pgtype.Text) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

func nonEmptyJSON(value json.RawMessage) json.RawMessage {
	if len(value) == 0 {
		return json.RawMessage(`{}`)
	}
	return value
}

func fromSQLDefinition(row sqlc.PluginDefinition) Definition {
	return Definition{ID: row.ID, Namespace: row.Namespace, DisplayName: row.DisplayName, Backend: Backend(row.Backend), Source: Source(row.Source), ImplementationKey: row.ImplementationKey, Spec: row.Spec, DefaultEnabled: row.DefaultEnabled, Revision: row.Revision, CreatorUserID: textValue(row.CreatorUserID), CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC()}
}

func fromSQLConfig(row sqlc.PluginConfig) Config {
	return Config{ID: row.ID, PluginID: row.PluginID, Namespace: row.Namespace, Scope: Scope(row.Scope), UserID: textValue(row.UserID), AgentID: textValue(row.AgentID), Enabled: boolValue(row.Enabled), Payload: row.Config, CredentialRefs: row.CredentialRefs, Revision: row.Revision, CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC()}
}

func mapNotFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func mapConflict(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "23503" || pgErr.Code == "23505") {
		return ErrConflict
	}
	return err
}
