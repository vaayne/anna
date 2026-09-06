-- name: ListVaultEntriesByScope :many
SELECT *
FROM vault_entry
WHERE scope = sqlc.arg(scope)
  AND coalesce(user_id::text, '') = coalesce(sqlc.narg(user_id)::text, '')
  AND coalesce(agent_id, '') = coalesce(sqlc.narg(agent_id), '')
ORDER BY name;

-- name: ListVaultEntriesForRuntime :many
SELECT *
FROM vault_entry
WHERE ((scope = 'system' AND user_id IS NULL AND vault_entry.agent_id IS NULL)
    OR (scope = 'system_agent' AND user_id IS NULL AND vault_entry.agent_id = sqlc.arg(runtime_agent_id))
    OR (scope = 'user' AND user_id = sqlc.arg(user_id) AND vault_entry.agent_id IS NULL)
    OR (scope = 'user_agent' AND user_id = sqlc.arg(user_id) AND vault_entry.agent_id = sqlc.arg(runtime_agent_id)))
-- Keep this precedence in sync with vault.Service.LoadEnvForAgent's merge loop.
ORDER BY CASE scope
    WHEN 'system' THEN 1
    WHEN 'system_agent' THEN 2
    WHEN 'user' THEN 3
    WHEN 'user_agent' THEN 4
    ELSE 0
END, name;

-- name: GetVaultEntryByScope :one
SELECT *
FROM vault_entry
WHERE scope = sqlc.arg(scope)
  AND coalesce(user_id::text, '') = coalesce(sqlc.narg(user_id)::text, '')
  AND coalesce(agent_id, '') = coalesce(sqlc.narg(agent_id), '')
  AND name = sqlc.arg(name);

-- name: UpsertVaultEntryByScope :one
INSERT INTO vault_entry (id, scope, user_id, agent_id, name, ciphertext, description)
VALUES ($1, $2, $3, $4, $5, $6, sqlc.narg(description))
ON CONFLICT (scope, (COALESCE(user_id::text, '')), (COALESCE(agent_id, '')), name) DO UPDATE SET
    ciphertext = excluded.ciphertext,
    description = coalesce(sqlc.narg(description), vault_entry.description),
    updated_at = now()
RETURNING *;

-- name: DeleteVaultEntryByScope :exec
DELETE FROM vault_entry
WHERE scope = sqlc.arg(scope)
  AND coalesce(user_id::text, '') = coalesce(sqlc.narg(user_id)::text, '')
  AND coalesce(agent_id, '') = coalesce(sqlc.narg(agent_id), '')
  AND name = sqlc.arg(name);


-- name: DeleteMCPConfigCredentials :exec
-- Names are derived here so the caller cannot broaden the deletion to a prefix.
DELETE FROM vault_entry
WHERE name IN (
    'MCP_TOKEN_' || upper(replace(sqlc.arg(config_id)::uuid::text, '-', '_')),
    'MCP_OAUTH_' || upper(replace(sqlc.arg(config_id)::uuid::text, '-', '_')),
    'MCP_OAUTH_CLIENT_' || upper(replace(sqlc.arg(config_id)::uuid::text, '-', '_'))
);
