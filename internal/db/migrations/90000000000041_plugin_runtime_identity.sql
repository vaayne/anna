-- Unified plugin runtime identity cutover.
--
-- This migration combines the mcp_connection_state preparation with the final
-- identity/FK cutover. It must be applied with every old stellad writer
-- stopped. The importer is a separate maintenance transaction after this
-- migration commits; it validates the NOT VALID OAuth FK immediately before
-- writing plugin_cutover_v1.

-- +goose Up
-- Keep the schema transaction bounded. A blocked maintenance migration must
-- fail and be retried, rather than wait behind an old writer indefinitely.
SET LOCAL lock_timeout = '5s';

-- Remote MCP observations are separate from authored plugin configuration.
-- Legacy mcp_server rows remain untouched until the runtime cutover.
CREATE TABLE mcp_connection_state (
    id UUID PRIMARY KEY DEFAULT uuidv7(),
    config_id UUID NOT NULL REFERENCES plugin_config(id) ON DELETE CASCADE,
    credential_user_id UUID REFERENCES auth_user(id) ON DELETE CASCADE,
    tools JSONB NOT NULL DEFAULT '[]'::jsonb,
    status TEXT NOT NULL DEFAULT 'unknown',
    status_error TEXT NOT NULL DEFAULT '',
    probed_at TIMESTAMPTZ,
    config_revision BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT mcp_connection_state_tools_array_check CHECK (jsonb_typeof(tools) = 'array'),
    CONSTRAINT mcp_connection_state_revision_check CHECK (config_revision > 0),
    CONSTRAINT mcp_connection_state_identity_key UNIQUE NULLS NOT DISTINCT (config_id, credential_user_id)
);

CREATE INDEX idx_mcp_connection_state_credential_user_id
    ON mcp_connection_state (credential_user_id)
    WHERE credential_user_id IS NOT NULL;

-- +goose StatementBegin
DO $$
BEGIN
    IF to_regclass('public.plugin_definition') IS NULL
       OR to_regclass('public.plugin_config') IS NULL
       OR to_regclass('public.mcp_connection_state') IS NULL
       OR to_regclass('public.mcp_oauth_flow') IS NULL
       OR to_regclass('public.tool_override') IS NULL THEN
        RAISE EXCEPTION 'plugin cutover requires migrations 40 and 41';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'tool_override'
          AND column_name = 'plugin_id'
    ) OR NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'tool_override'
          AND column_name = 'local_tool_name'
    ) THEN
        RAISE EXCEPTION 'plugin cutover requires tool identity columns from migration 40';
    END IF;
END
$$;
-- +goose StatementEnd

-- There must be one existing single-column FK on server_id at the boundary.
-- The candidate knows the old and reviewed new names; an extra legacy edge is
-- ambiguous and must stop the migration before either edge is dropped.
-- +goose StatementBegin
DO $$
BEGIN
    IF (
        SELECT count(*)
        FROM pg_constraint c
        JOIN pg_class child ON child.oid = c.conrelid
        JOIN pg_attribute child_col ON child_col.attrelid = child.oid
        WHERE c.contype = 'f'
          AND child.relnamespace = 'public'::regnamespace
          AND child.relname = 'mcp_oauth_flow'
          AND child_col.attname = 'server_id'
          AND cardinality(c.conkey) = 1
          AND c.conkey[1] = child_col.attnum
    ) > 1 THEN
        RAISE EXCEPTION 'mcp_oauth_flow.server_id has additional foreign keys';
    END IF;
END
$$;
-- +goose StatementEnd

-- Fail before changing identity constraints if a prior writer left a row in
-- neither of the two legal identity shapes. The old migration-40 CHECK was
-- intentionally weaker so the legacy name could remain during preparation.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM tool_override
        WHERE NOT (
            (plugin_id IS NULL AND local_tool_name IS NULL
             AND tool_name IS NOT NULL AND tool_name <> '')
            OR
            (plugin_id IS NOT NULL AND local_tool_name IS NOT NULL
             AND plugin_id <> '' AND local_tool_name <> '' AND tool_name IS NULL)
        )
    ) THEN
        RAISE EXCEPTION 'tool_override contains an invalid dual identity row';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE tool_override ALTER COLUMN tool_name DROP NOT NULL;
ALTER TABLE tool_override DROP CONSTRAINT IF EXISTS tool_override_tool_name_scope_user_id_agent_id_key;
ALTER TABLE tool_override DROP CONSTRAINT IF EXISTS tool_override_plugin_identity_pair;
ALTER TABLE tool_override DROP CONSTRAINT IF EXISTS tool_override_identity_check;
ALTER TABLE tool_override
    ADD CONSTRAINT tool_override_identity_check CHECK (
        (plugin_id IS NULL AND local_tool_name IS NULL
         AND tool_name IS NOT NULL AND tool_name <> '')
        OR
        (plugin_id IS NOT NULL AND local_tool_name IS NOT NULL
         AND plugin_id <> '' AND local_tool_name <> '' AND tool_name IS NULL)
    );

CREATE UNIQUE INDEX uniq_tool_override_core_identity
    ON tool_override (tool_name, scope, user_id, agent_id) NULLS NOT DISTINCT
    WHERE tool_name IS NOT NULL AND plugin_id IS NULL AND local_tool_name IS NULL;
CREATE UNIQUE INDEX uniq_tool_override_plugin_identity
    ON tool_override (plugin_id, local_tool_name, scope, user_id, agent_id) NULLS NOT DISTINCT
    WHERE tool_name IS NULL;

-- The old FK names the legacy mcp_server table. Remove that writer-era edge,
-- then install the final UUID-preserving edge without scanning old rows yet.
-- Existing flow rows still point to the legacy UUID and are validated only
-- after ImportLegacyState has inserted the matching plugin_config rows.
ALTER TABLE public.mcp_oauth_flow DROP CONSTRAINT IF EXISTS mcp_oauth_flow_server_id_fkey;
ALTER TABLE public.mcp_oauth_flow DROP CONSTRAINT IF EXISTS mcp_oauth_flow_server_id_plugin_config_fkey;
ALTER TABLE public.mcp_oauth_flow
    ADD CONSTRAINT mcp_oauth_flow_server_id_plugin_config_fkey
    FOREIGN KEY (server_id) REFERENCES public.plugin_config(id)
    ON DELETE CASCADE NOT VALID;

-- +goose Down
-- This is a maintenance cutover boundary, not a reversible product change.
-- A down migration must never detach committed OAuth flows or plugin policy.
-- +goose StatementBegin
DO $$
BEGIN
    RAISE EXCEPTION 'migration 90000000000041 is irreversible after cutover';
END
$$;
-- +goose StatementEnd
