-- +goose Up

-- All statements in this migration run in Goose's transaction. These
-- checks must precede every DELETE so an unexpected/custom row rolls back the
-- whole migration and remains inspectable.
-- +goose StatementBegin
DO $$
DECLARE
    bad text;
BEGIN
    -- Legacy plugin rows must still be the shipped tool identity. A malformed
    -- config is not silently discarded with the row.
    SELECT string_agg(format('%s(kind=%s,name=%s)', id, kind, name), ', ' ORDER BY id)
      INTO bad
      FROM plugin
     WHERE id IN ('tool/mise', 'tool/xberg', 'tool/fd', 'tool/rg')
       AND (kind <> 'tool'
            OR name <> split_part(id, '/', 2)
            OR jsonb_typeof(config) <> 'object');
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'retired builtin plugin has unexpected legacy shape: %', bad;
    END IF;

    -- Common definitions must be the release-owned identities. Display name
    -- and revision are intentionally not used as identity checks.
    SELECT string_agg(format('%s(namespace=%s,backend=%s,source=%s,key=%s)',
                             id, namespace, backend, source, implementation_key), ', ' ORDER BY id)
      INTO bad
      FROM plugin_definition
     WHERE id IN ('tool/mise', 'tool/xberg', 'tool/fd', 'tool/rg')
       AND (source <> 'builtin'
            OR namespace <> CASE id
                WHEN 'tool/mise' THEN 'mise'
                WHEN 'tool/xberg' THEN 'xberg'
                WHEN 'tool/fd' THEN 'fd'
                WHEN 'tool/rg' THEN 'rg'
            END
            OR backend <> CASE id WHEN 'tool/xberg' THEN 'go' ELSE 'cli' END
            OR implementation_key <> id
            OR creator_user_id IS NOT NULL
            OR jsonb_typeof(spec) <> 'object');
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'retired builtin definition has unexpected shape: %', bad;
    END IF;

    -- These IDs are runtime roots. A dependency declaration would be data the
    -- cleanup does not understand, so fail before removing its definition.
    IF EXISTS (
        SELECT 1 FROM plugin_definition
         WHERE id IN ('tool/mise', 'tool/xberg', 'tool/fd', 'tool/rg')
           AND spec ? 'requires_plugin_ids'
    ) THEN
        RAISE EXCEPTION 'retired builtin definition has unsupported plugin dependencies';
    END IF;

    -- A different definition must not depend on an ID that this migration is
    -- about to remove. Walk every JSON object/array so a nested dependency
    -- declaration cannot evade the guard. Scalar equality avoids matching
    -- harmless prose that merely mentions a tool name.
    WITH RECURSIVE walk(owner_id, value) AS (
        SELECT id, spec
          FROM plugin_definition
         WHERE jsonb_typeof(spec) = 'object'
        UNION ALL
        SELECT walk.owner_id, child.value
          FROM walk
          CROSS JOIN LATERAL (
              SELECT value
                FROM jsonb_each(
                    CASE WHEN jsonb_typeof(walk.value) = 'object'
                         THEN walk.value ELSE '{}'::jsonb END
                )
              UNION ALL
              SELECT value
                FROM jsonb_array_elements(
                    CASE WHEN jsonb_typeof(walk.value) = 'array'
                         THEN walk.value ELSE '[]'::jsonb END
                )
          ) AS child(value)
    )
    SELECT string_agg(DISTINCT owner_id, ', ' ORDER BY owner_id)
      INTO bad
      FROM walk
     WHERE value #>> '{}' IN ('tool/mise', 'tool/xberg', 'tool/fd', 'tool/rg');
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'definition specs depend on retired builtin IDs: %', bad;
    END IF;

    -- These are CLI/Go roots, never MCP registrations. A credential-bearing
    -- child means the imported state is not the shape this cleanup owns; fail
    -- instead of relying on ON DELETE CASCADE to hide it.
    IF EXISTS (
        SELECT 1
          FROM plugin_config c
          JOIN mcp_connection_state s ON s.config_id = c.id
         WHERE c.plugin_id IN ('tool/mise', 'tool/xberg', 'tool/fd', 'tool/rg')
    ) OR EXISTS (
        SELECT 1
          FROM plugin_config c
          JOIN mcp_oauth_flow f ON f.server_id = c.id
         WHERE c.plugin_id IN ('tool/mise', 'tool/xberg', 'tool/fd', 'tool/rg')
    ) THEN
        RAISE EXCEPTION 'retired builtin config has MCP observation or OAuth flow children';
    END IF;

    -- A non-empty locator is still a credential reference even when no child
    -- row exists. The retirement migration has no vault move operation, so it
    -- must stop before deleting a config that would strand that locator.
    IF EXISTS (
        SELECT 1
          FROM plugin_config
         WHERE plugin_id IN ('tool/mise', 'tool/xberg', 'tool/fd', 'tool/rg')
           AND credential_refs <> '{}'::jsonb
    ) THEN
        RAISE EXCEPTION 'retired builtin config contains credential references';
    END IF;

    -- Likewise, the old manifest row may point at a vault-backed session-env
    -- blob. It cannot be moved by this schema-only cleanup.
    IF EXISTS (
        SELECT 1
          FROM plugin_override
         WHERE plugin_id IN ('tool/mise', 'tool/xberg', 'tool/fd', 'tool/rg')
           AND session_env_vault_key <> ''
    ) THEN
        RAISE EXCEPTION 'retired builtin manifest override contains a vault locator';
    END IF;

    -- Non-empty manifest overrides must at least be readable JSON objects.
    -- The cast deliberately aborts the Goose transaction on malformed JSON.
    IF EXISTS (
        SELECT 1 FROM plugin_override
         WHERE plugin_id IN ('tool/mise', 'tool/xberg', 'tool/fd', 'tool/rg')
           AND config <> ''
    ) THEN
        PERFORM config::jsonb
          FROM plugin_override
         WHERE plugin_id IN ('tool/mise', 'tool/xberg', 'tool/fd', 'tool/rg')
           AND config <> ''
           AND jsonb_typeof(config::jsonb) <> 'object';
        IF FOUND THEN
            RAISE EXCEPTION 'retired builtin manifest override is not a JSON object';
        END IF;
    END IF;
END
$$;
-- +goose StatementEnd

-- Delete children before RESTRICT definition FKs. The guards above establish
-- that target configs have no observation/OAuth children and no credential
-- locators, so this cleanup cannot hide or strand runtime state. No
-- mcp_server or plugin_oauth_provider row is in this target set.
DELETE FROM tool_override
 WHERE plugin_id IN ('tool/mise', 'tool/xberg', 'tool/fd', 'tool/rg');
DELETE FROM plugin_config
 WHERE plugin_id IN ('tool/mise', 'tool/xberg', 'tool/fd', 'tool/rg');
DELETE FROM plugin_definition
 WHERE id IN ('tool/mise', 'tool/xberg', 'tool/fd', 'tool/rg');
DELETE FROM plugin_state
 WHERE plugin_id IN ('tool/mise', 'tool/xberg', 'tool/fd', 'tool/rg');
DELETE FROM plugin_override
 WHERE plugin_id IN ('tool/mise', 'tool/xberg', 'tool/fd', 'tool/rg');
DELETE FROM plugin
 WHERE id IN ('tool/mise', 'tool/xberg', 'tool/fd', 'tool/rg');

-- +goose Down
-- Cleanup is forward-only and does not reconstruct
-- release-owned definitions or user policy.
-- +goose StatementBegin
DO $$
BEGIN
    RAISE EXCEPTION 'migration 90000000000042 is irreversible after retirement';
END
$$;
-- +goose StatementEnd
