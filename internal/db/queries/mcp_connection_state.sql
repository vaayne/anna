-- name: ListMCPConnectionStatesForConfigs :many
SELECT id, config_id, credential_user_id, tools, status, status_error,
       probed_at, config_revision, created_at, updated_at
FROM mcp_connection_state
WHERE config_id = ANY(sqlc.arg(config_ids)::uuid[])
  AND (
      credential_user_id IS NULL
      OR credential_user_id = sqlc.narg(credential_user_id)::uuid
  )
ORDER BY array_position(sqlc.arg(config_ids)::uuid[], config_id), credential_user_id NULLS FIRST, id;

-- name: LockMCPConfigRevision :one
SELECT revision
FROM plugin_config
WHERE id = sqlc.arg(config_id)::uuid
FOR UPDATE;

-- name: UpsertMCPConnectionState :one
INSERT INTO mcp_connection_state (
    config_id, credential_user_id, tools, status, status_error,
    probed_at, config_revision
)
VALUES (
    sqlc.arg(config_id)::uuid,
    sqlc.narg(credential_user_id)::uuid,
    sqlc.arg(tools)::jsonb,
    sqlc.arg(status),
    sqlc.arg(status_error),
    sqlc.narg(probed_at),
    sqlc.arg(config_revision)
)
ON CONFLICT (config_id, credential_user_id) DO UPDATE
SET tools = EXCLUDED.tools,
    status = EXCLUDED.status,
    status_error = EXCLUDED.status_error,
    probed_at = EXCLUDED.probed_at,
    config_revision = EXCLUDED.config_revision,
    updated_at = now()
RETURNING id, config_id, credential_user_id, tools, status, status_error,
          probed_at, config_revision, created_at, updated_at;
