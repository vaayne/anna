-- ListToolOverridesForAgentContext returns rows visible to one user and agent.
-- name: ListToolOverridesForAgentContext :many
SELECT * FROM tool_override
WHERE scope = 'system'
   OR (scope = 'system_agent' AND agent_id = sqlc.narg(agent_id))
   OR (scope = 'user'         AND user_id = sqlc.narg(user_id))
   OR (scope = 'user_agent'   AND user_id = sqlc.narg(user_id) AND agent_id = sqlc.narg(agent_id))
ORDER BY CASE scope
    WHEN 'user_agent'   THEN 1
    WHEN 'user'         THEN 2
    WHEN 'system_agent' THEN 3
    WHEN 'system'       THEN 4
  END, tool_name;

-- name: GetToolOverride :one
SELECT * FROM tool_override
WHERE tool_name = sqlc.arg(tool_name)
  AND scope = sqlc.arg(scope)
  AND coalesce(user_id::text, '') = coalesce(sqlc.narg(user_id)::text, '')
  AND coalesce(agent_id, '') = coalesce(sqlc.narg(agent_id), '')
LIMIT 1;

-- name: GetToolOverrideByIdentity :one
SELECT * FROM tool_override
WHERE tool_name IS NOT DISTINCT FROM sqlc.narg(tool_name)
  AND plugin_id IS NOT DISTINCT FROM sqlc.narg(plugin_id)
  AND local_tool_name IS NOT DISTINCT FROM sqlc.narg(local_tool_name)
  AND scope = sqlc.arg(scope)
  AND coalesce(user_id::text, '') = coalesce(sqlc.narg(user_id)::text, '')
  AND coalesce(agent_id, '') = coalesce(sqlc.narg(agent_id), '')
LIMIT 1;

-- name: UpsertCoreToolOverride :one
INSERT INTO tool_override (tool_name, plugin_id, local_tool_name, scope, user_id, agent_id, enabled)
VALUES (sqlc.arg(tool_name), NULL, NULL, sqlc.arg(scope), sqlc.narg(user_id), sqlc.narg(agent_id), sqlc.arg(enabled))
ON CONFLICT (tool_name, scope, user_id, agent_id)
  WHERE tool_name IS NOT NULL AND plugin_id IS NULL AND local_tool_name IS NULL
DO UPDATE SET enabled = excluded.enabled, updated_at = now()
RETURNING *;

-- name: UpsertPluginToolOverride :one
INSERT INTO tool_override (tool_name, plugin_id, local_tool_name, scope, user_id, agent_id, enabled)
VALUES (NULL, sqlc.arg(plugin_id), sqlc.arg(local_tool_name), sqlc.arg(scope), sqlc.narg(user_id), sqlc.narg(agent_id), sqlc.arg(enabled))
ON CONFLICT (plugin_id, local_tool_name, scope, user_id, agent_id)
  WHERE tool_name IS NULL
DO UPDATE SET enabled = excluded.enabled, updated_at = now()
RETURNING *;

-- name: InsertCoreToolOverrideIfAbsent :one
INSERT INTO tool_override (tool_name, plugin_id, local_tool_name, scope, user_id, agent_id, enabled)
VALUES (sqlc.arg(tool_name), NULL, NULL, sqlc.arg(scope), sqlc.narg(user_id), sqlc.narg(agent_id), sqlc.arg(enabled))
ON CONFLICT DO NOTHING
RETURNING *;

-- name: InsertPluginToolOverrideIfAbsent :one
INSERT INTO tool_override (tool_name, plugin_id, local_tool_name, scope, user_id, agent_id, enabled)
VALUES (NULL, sqlc.arg(plugin_id), sqlc.arg(local_tool_name), sqlc.arg(scope), sqlc.narg(user_id), sqlc.narg(agent_id), sqlc.arg(enabled))
ON CONFLICT DO NOTHING
RETURNING *;

-- name: UpdateCoreToolOverrideIfVersion :one
UPDATE tool_override
SET enabled = sqlc.arg(enabled), updated_at = now()
WHERE tool_name = sqlc.arg(tool_name)
  AND plugin_id IS NULL AND local_tool_name IS NULL
  AND scope = sqlc.arg(scope)
  AND coalesce(user_id::text, '') = coalesce(sqlc.narg(user_id)::text, '')
  AND coalesce(agent_id, '') = coalesce(sqlc.narg(agent_id), '')
  AND updated_at = sqlc.arg(expected_updated_at)
RETURNING *;

-- name: UpdatePluginToolOverrideIfVersion :one
UPDATE tool_override
SET enabled = sqlc.arg(enabled), updated_at = now()
WHERE tool_name IS NULL
  AND plugin_id = sqlc.arg(plugin_id)
  AND local_tool_name = sqlc.arg(local_tool_name)
  AND scope = sqlc.arg(scope)
  AND coalesce(user_id::text, '') = coalesce(sqlc.narg(user_id)::text, '')
  AND coalesce(agent_id, '') = coalesce(sqlc.narg(agent_id), '')
  AND updated_at = sqlc.arg(expected_updated_at)
RETURNING *;

-- name: DeleteCoreToolOverride :exec
DELETE FROM tool_override
WHERE tool_name = sqlc.arg(tool_name)
  AND plugin_id IS NULL AND local_tool_name IS NULL
  AND scope = sqlc.arg(scope)
  AND coalesce(user_id::text, '') = coalesce(sqlc.narg(user_id)::text, '')
  AND coalesce(agent_id, '') = coalesce(sqlc.narg(agent_id), '');

-- name: DeletePluginToolOverride :exec
DELETE FROM tool_override
WHERE tool_name IS NULL
  AND plugin_id = sqlc.arg(plugin_id)
  AND local_tool_name = sqlc.arg(local_tool_name)
  AND scope = sqlc.arg(scope)
  AND coalesce(user_id::text, '') = coalesce(sqlc.narg(user_id)::text, '')
  AND coalesce(agent_id, '') = coalesce(sqlc.narg(agent_id), '');

-- name: DeleteCoreToolOverrideIfVersion :one
DELETE FROM tool_override
WHERE tool_name = sqlc.arg(tool_name)
  AND plugin_id IS NULL AND local_tool_name IS NULL
  AND scope = sqlc.arg(scope)
  AND coalesce(user_id::text, '') = coalesce(sqlc.narg(user_id)::text, '')
  AND coalesce(agent_id, '') = coalesce(sqlc.narg(agent_id), '')
  AND updated_at = sqlc.arg(expected_updated_at)
RETURNING *;

-- name: DeletePluginToolOverrideIfVersion :one
DELETE FROM tool_override
WHERE tool_name IS NULL
  AND plugin_id = sqlc.arg(plugin_id)
  AND local_tool_name = sqlc.arg(local_tool_name)
  AND scope = sqlc.arg(scope)
  AND coalesce(user_id::text, '') = coalesce(sqlc.narg(user_id)::text, '')
  AND coalesce(agent_id, '') = coalesce(sqlc.narg(agent_id), '')
  AND updated_at = sqlc.arg(expected_updated_at)
RETURNING *;

-- name: UpsertToolOverride :one
INSERT INTO tool_override (tool_name, scope, user_id, agent_id, enabled)
VALUES (sqlc.arg(tool_name), sqlc.arg(scope), sqlc.narg(user_id), sqlc.narg(agent_id), sqlc.arg(enabled))
ON CONFLICT (tool_name, scope, user_id, agent_id)
DO UPDATE SET enabled = excluded.enabled, updated_at = now()
RETURNING *;

-- name: InsertToolOverrideIfAbsent :one
INSERT INTO tool_override (tool_name, scope, user_id, agent_id, enabled)
VALUES (sqlc.arg(tool_name), sqlc.arg(scope), sqlc.narg(user_id), sqlc.narg(agent_id), sqlc.arg(enabled))
ON CONFLICT DO NOTHING
RETURNING *;

-- name: UpdateToolOverrideIfVersion :one
UPDATE tool_override
SET enabled = sqlc.arg(enabled), updated_at = now()
WHERE tool_name = sqlc.arg(tool_name)
  AND scope = sqlc.arg(scope)
  AND coalesce(user_id::text, '') = coalesce(sqlc.narg(user_id)::text, '')
  AND coalesce(agent_id, '') = coalesce(sqlc.narg(agent_id), '')
  AND updated_at = sqlc.arg(expected_updated_at)
RETURNING *;

-- name: DeleteToolOverride :exec
DELETE FROM tool_override
WHERE tool_name = sqlc.arg(tool_name)
  AND scope = sqlc.arg(scope)
  AND coalesce(user_id::text, '') = coalesce(sqlc.narg(user_id)::text, '')
  AND coalesce(agent_id, '') = coalesce(sqlc.narg(agent_id), '');

-- name: DeleteToolOverrideIfVersion :one
DELETE FROM tool_override
WHERE tool_name = sqlc.arg(tool_name)
  AND scope = sqlc.arg(scope)
  AND coalesce(user_id::text, '') = coalesce(sqlc.narg(user_id)::text, '')
  AND coalesce(agent_id, '') = coalesce(sqlc.narg(agent_id), '')
  AND updated_at = sqlc.arg(expected_updated_at)
RETURNING *;

-- RenameToolOverridePrefix rewrites every override row whose tool_name starts
-- with old_prefix to start with new_prefix instead. It is owner-unscoped on
-- purpose: a system registration's rename must migrate every user's override
-- on that server's tools. The prefix comparison uses substr equality rather
-- than LIKE so the underscores inside "mcp__" are never read as wildcards.
-- name: RenameToolOverridePrefix :exec
UPDATE tool_override
SET tool_name = sqlc.arg(new_prefix)::text || substr(tool_name, length(sqlc.arg(old_prefix)::text) + 1)
WHERE substr(tool_name, 1, length(sqlc.arg(old_prefix)::text)) = sqlc.arg(old_prefix)::text;

-- name: DeleteToolOverridesByPrefix :execrows
DELETE FROM tool_override
WHERE substr(tool_name, 1, length(sqlc.arg(prefix)::text)) = sqlc.arg(prefix)::text;
