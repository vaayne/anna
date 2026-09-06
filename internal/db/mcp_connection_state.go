package db

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrMCPConnectionConfigNotFound means the observation's parent config was
	// deleted before the observation transaction could lock it.
	ErrMCPConnectionConfigNotFound = errors.New("mcp connection state: config not found")
	// ErrMCPConnectionStateStale means the observation was produced for an old
	// plugin_config revision. The caller must discard it and probe the new config.
	ErrMCPConnectionStateStale = errors.New("mcp connection state: stale config revision")
)

// MCPConnectionState is a remote MCP observation keyed by one plugin config
// and, for per-user credentials, one trusted credential owner. It contains no
// authored endpoint or credential data.
type MCPConnectionState struct {
	ID               string
	ConfigID         string
	CredentialUserID *string
	Tools            json.RawMessage
	Status           string
	StatusError      string
	ProbedAt         *time.Time
	ConfigRevision   int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

const listMCPConnectionStatesSQL = `
SELECT id, config_id, credential_user_id, tools, status, status_error,
       probed_at, config_revision, created_at, updated_at
FROM mcp_connection_state
WHERE config_id = ANY($1::uuid[])
  AND (credential_user_id IS NULL OR credential_user_id = $2::uuid)
ORDER BY array_position($1::uuid[], config_id), credential_user_id NULLS FIRST, id`

const lockMCPConfigRevisionSQL = `
SELECT revision
FROM plugin_config
WHERE id = $1::uuid
FOR UPDATE`

const upsertMCPConnectionStateSQL = `
INSERT INTO mcp_connection_state (
    config_id, credential_user_id, tools, status, status_error,
    probed_at, config_revision
)
VALUES ($1::uuid, $2::uuid, $3::jsonb, $4, $5, $6, $7)
ON CONFLICT (config_id, credential_user_id) DO UPDATE
SET tools = EXCLUDED.tools,
    status = EXCLUDED.status,
    status_error = EXCLUDED.status_error,
    probed_at = EXCLUDED.probed_at,
    config_revision = EXCLUDED.config_revision,
    updated_at = now()
RETURNING id, config_id, credential_user_id, tools, status, status_error,
          probed_at, config_revision, created_at, updated_at`

// ListMCPConnectionStatesForConfigs reads only the selected config IDs. A
// non-nil trusted user ID includes shared state and that user's per-user state;
// nil reads shared state only. Empty IDs avoid an accidental broad read.
func ListMCPConnectionStatesForConfigs(ctx context.Context, pool *pgxpool.Pool, configIDs []string, credentialUserID *string) ([]MCPConnectionState, error) {
	if pool == nil {
		return nil, errors.New("mcp connection state: nil database")
	}
	if len(configIDs) == 0 {
		return nil, nil
	}
	for _, configID := range configIDs {
		if _, err := uuid.Parse(configID); err != nil {
			return nil, fmt.Errorf("mcp connection state: invalid config id: %w", err)
		}
	}
	var owner any
	if credentialUserID != nil {
		if _, err := uuid.Parse(*credentialUserID); err != nil {
			return nil, fmt.Errorf("mcp connection state: invalid credential user id: %w", err)
		}
		owner = *credentialUserID
	}
	rows, err := pool.Query(ctx, listMCPConnectionStatesSQL, configIDs, owner)
	if err != nil {
		return nil, fmt.Errorf("list MCP connection states: %w", err)
	}
	defer rows.Close()
	states := make([]MCPConnectionState, 0, len(configIDs))
	for rows.Next() {
		state, err := scanMCPConnectionState(rows)
		if err != nil {
			return nil, fmt.Errorf("scan MCP connection state: %w", err)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list MCP connection states: %w", err)
	}
	return states, nil
}

// StoreMCPConnectionState writes one observation inside the caller's
// transaction. Locking the parent config and checking its revision makes both
// the first insert and replacement obey the same fence. The function never
// commits or rolls back tx.
func StoreMCPConnectionState(ctx context.Context, tx pgx.Tx, state MCPConnectionState) (MCPConnectionState, error) {
	if tx == nil {
		return MCPConnectionState{}, errors.New("mcp connection state: nil transaction")
	}
	if _, err := uuid.Parse(state.ConfigID); err != nil {
		return MCPConnectionState{}, fmt.Errorf("mcp connection state: invalid config id: %w", err)
	}
	if state.CredentialUserID != nil {
		if _, err := uuid.Parse(*state.CredentialUserID); err != nil {
			return MCPConnectionState{}, fmt.Errorf("mcp connection state: invalid credential user id: %w", err)
		}
	}
	if state.ConfigRevision < 1 {
		return MCPConnectionState{}, errors.New("mcp connection state: config revision must be positive")
	}
	toolsJSON, err := observationToolsJSON(state.Tools)
	if err != nil {
		return MCPConnectionState{}, err
	}
	if state.Status == "" {
		state.Status = "unknown"
	}
	var parentRevision int64
	if err := tx.QueryRow(ctx, lockMCPConfigRevisionSQL, state.ConfigID).Scan(&parentRevision); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return MCPConnectionState{}, ErrMCPConnectionConfigNotFound
		}
		return MCPConnectionState{}, fmt.Errorf("lock MCP config revision: %w", err)
	}
	if parentRevision != state.ConfigRevision {
		return MCPConnectionState{}, fmt.Errorf("%w: got %d, current %d", ErrMCPConnectionStateStale, state.ConfigRevision, parentRevision)
	}
	var owner any
	if state.CredentialUserID != nil {
		owner = *state.CredentialUserID
	}
	var probedAt any
	if state.ProbedAt != nil {
		probedAt = state.ProbedAt.UTC()
	}
	row := tx.QueryRow(ctx, upsertMCPConnectionStateSQL, state.ConfigID, owner, toolsJSON, state.Status, state.StatusError, probedAt, state.ConfigRevision)
	stored, err := scanMCPConnectionState(row)
	if err != nil {
		return MCPConnectionState{}, fmt.Errorf("upsert MCP connection state: %w", err)
	}
	return stored, nil
}

func observationToolsJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage(`[]`), nil
	}
	trimmed := bytes.TrimSpace(raw)
	if trimmed[0] != '[' {
		return nil, errors.New("mcp connection state: tools must be a JSON array")
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(trimmed, &tools); err != nil {
		return nil, fmt.Errorf("mcp connection state: invalid tools JSON: %w", err)
	}
	return json.RawMessage(bytes.Clone(trimmed)), nil
}

type mcpConnectionStateRowScanner interface {
	Scan(dest ...any) error
}

func scanMCPConnectionState(row mcpConnectionStateRowScanner) (MCPConnectionState, error) {
	var (
		state     MCPConnectionState
		owner     pgtype.Text
		probedAt  pgtype.Timestamptz
		createdAt time.Time
		updatedAt time.Time
	)
	if err := row.Scan(
		&state.ID, &state.ConfigID, &owner, &state.Tools, &state.Status,
		&state.StatusError, &probedAt, &state.ConfigRevision, &createdAt, &updatedAt,
	); err != nil {
		return MCPConnectionState{}, err
	}
	if owner.Valid {
		value := owner.String
		state.CredentialUserID = &value
	}
	if probedAt.Valid {
		value := probedAt.Time.UTC()
		state.ProbedAt = &value
	}
	state.CreatedAt = createdAt.UTC()
	state.UpdatedAt = updatedAt.UTC()
	return state, nil
}
