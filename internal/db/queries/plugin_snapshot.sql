-- name: ListPluginConfigsForContext :many
SELECT * FROM plugin_config
WHERE scope = 'system'
   OR (scope = 'system_agent' AND agent_id = sqlc.narg(agent_id))
   OR (scope = 'user' AND user_id = sqlc.narg(user_id)::uuid)
   OR (scope = 'user_agent' AND user_id = sqlc.narg(user_id)::uuid AND agent_id = sqlc.narg(agent_id))
ORDER BY plugin_id, scope, user_id NULLS FIRST, agent_id NULLS FIRST, id;
