-- Unified plugin definitions and four-scope configuration records. These
-- queries are intentionally separate from legacy plugin rows until cutover.

-- name: GetPluginDefinition :one
SELECT * FROM plugin_definition WHERE id = $1;

-- name: ListPluginDefinitions :many
SELECT * FROM plugin_definition ORDER BY namespace, id;

-- name: UpsertPluginDefinition :one
INSERT INTO plugin_definition (
    id, namespace, display_name, backend, source, implementation_key,
    spec, default_enabled, revision, creator_user_id, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now())
ON CONFLICT (id) DO UPDATE SET
    display_name = excluded.display_name,
    spec = excluded.spec,
    default_enabled = excluded.default_enabled,
    revision = CASE WHEN (plugin_definition.display_name, plugin_definition.spec, plugin_definition.default_enabled)
        IS DISTINCT FROM (excluded.display_name, excluded.spec, excluded.default_enabled)
        THEN plugin_definition.revision + 1 ELSE plugin_definition.revision END,
    updated_at = CASE WHEN (plugin_definition.display_name, plugin_definition.spec, plugin_definition.default_enabled)
        IS DISTINCT FROM (excluded.display_name, excluded.spec, excluded.default_enabled)
        THEN now() ELSE plugin_definition.updated_at END
WHERE plugin_definition.namespace = excluded.namespace
  AND plugin_definition.backend = excluded.backend
  AND plugin_definition.source = excluded.source
  AND plugin_definition.implementation_key = excluded.implementation_key
RETURNING *;

-- name: ListPluginConfigs :many
SELECT * FROM plugin_config
WHERE plugin_id = $1
ORDER BY scope, user_id NULLS FIRST, agent_id NULLS FIRST, id;

-- name: ListPluginConfigsOwned :many
SELECT * FROM plugin_config
WHERE plugin_id = $1
  AND scope = $2
  AND user_id IS NOT DISTINCT FROM $3
  AND agent_id IS NOT DISTINCT FROM $4
ORDER BY id;

-- name: CreatePluginDefinition :one
INSERT INTO plugin_definition (
    id, namespace, display_name, backend, source, implementation_key,
    spec, default_enabled, revision, creator_user_id, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now())
RETURNING *;

-- name: UpdatePluginDefinitionCAS :one
UPDATE plugin_definition
SET display_name = $2,
    spec = $4,
    revision = revision + 1,
    updated_at = now()
WHERE id = $1 AND revision = $3 AND source = 'custom'
RETURNING *;

-- name: DeletePluginDefinitionCAS :execrows
DELETE FROM plugin_definition
WHERE id = $1 AND revision = $2 AND source = 'custom';

-- name: CreatePluginConfig :one
INSERT INTO plugin_config (
    id, plugin_id, namespace, scope, user_id, agent_id, enabled, config,
    credential_refs, revision, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now())
RETURNING *;

-- name: UpdatePluginConfigCAS :one
UPDATE plugin_config
SET enabled = $2,
    config = $3,
    credential_refs = $4,
    revision = revision + 1,
    updated_at = now()
WHERE id = $1 AND revision = $5
RETURNING *;

-- name: MovePluginConfigCAS :one
UPDATE plugin_config
SET scope = $2,
    user_id = $3,
    agent_id = $4,
    enabled = $5,
    config = $6,
    credential_refs = $7,
    revision = revision + 1,
    updated_at = now()
WHERE id = $1 AND revision = $8
RETURNING *;

-- name: DeletePluginConfigCAS :execrows
DELETE FROM plugin_config
USING plugin_definition
WHERE plugin_config.id = $1
  AND plugin_config.revision = $2
  AND plugin_config.plugin_id = $3
  AND plugin_config.plugin_id = plugin_definition.id
  AND NOT (plugin_definition.source = 'builtin' AND plugin_config.scope = 'system');

-- name: ResetBuiltinPluginConfig :one
UPDATE plugin_config
SET enabled = NULL,
    config = '{}'::jsonb,
    credential_refs = '{}'::jsonb,
    revision = revision + 1,
    updated_at = now()
WHERE id = $1 AND revision = $2 AND plugin_id = $3 AND scope = 'system'
RETURNING *;

-- name: EnsureSystemPluginConfig :one
WITH inserted AS (
    INSERT INTO plugin_config (
        plugin_id, namespace, scope, enabled, config, credential_refs, revision, updated_at
    ) VALUES ($1, $2, 'system', NULL, '{}'::jsonb, '{}'::jsonb, 1, now())
    ON CONFLICT (plugin_id, scope, user_id, agent_id) DO NOTHING
    RETURNING *
)
SELECT * FROM inserted
UNION ALL
SELECT * FROM plugin_config
WHERE plugin_id = $1 AND namespace = $2 AND scope = 'system'
LIMIT 1;

-- name: DeletePluginToolPolicies :exec
DELETE FROM tool_override WHERE plugin_id = $1;

-- name: GetPluginConfigForOwner :one
SELECT * FROM plugin_config
WHERE id = sqlc.arg(id) AND plugin_id = sqlc.arg(plugin_id)
  AND ((scope IN ('system', 'system_agent') AND sqlc.arg(is_admin)::boolean)
    OR (scope IN ('user', 'user_agent') AND user_id = sqlc.arg(viewer_user_id)::uuid));

-- name: LockPluginCatalog :exec
SELECT pg_advisory_xact_lock(hashtextextended('plugin_cutover_v1', 0));
