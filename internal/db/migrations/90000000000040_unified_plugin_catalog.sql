-- +goose Up

-- Additive catalog. Legacy plugin tables remain until the maintenance-style
-- cutover imports them and records its durable marker.
CREATE TABLE "plugin_definition" (
    "id" TEXT PRIMARY KEY,
    "namespace" TEXT NOT NULL,
    "display_name" TEXT NOT NULL,
    "backend" TEXT NOT NULL,
    "source" TEXT NOT NULL,
    "implementation_key" TEXT NOT NULL,
    "spec" JSONB NOT NULL DEFAULT '{}',
    "default_enabled" BOOLEAN NOT NULL DEFAULT false,
    "revision" BIGINT NOT NULL DEFAULT 1,
    "creator_user_id" UUID REFERENCES "auth_user" ("id") ON DELETE RESTRICT,
    "created_at" TIMESTAMPTZ NOT NULL DEFAULT now(),
    "updated_at" TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT "plugin_definition_identity_key" UNIQUE ("id", "namespace"),
    CONSTRAINT "plugin_definition_revision_check" CHECK ("revision" > 0),
    CONSTRAINT "plugin_definition_spec_object_check" CHECK (jsonb_typeof("spec") = 'object'),
    CONSTRAINT "plugin_definition_builtin_creator_check" CHECK ("source" <> 'builtin' OR "creator_user_id" IS NULL)
);
CREATE INDEX "idx_plugin_definition_creator_user_id" ON "plugin_definition" ("creator_user_id");

CREATE TABLE "plugin_config" (
    "id" UUID PRIMARY KEY DEFAULT uuidv7(),
    "plugin_id" TEXT NOT NULL,
    "namespace" TEXT NOT NULL,
    "scope" TEXT NOT NULL,
    "user_id" UUID REFERENCES "auth_user" ("id") ON DELETE CASCADE,
    "agent_id" TEXT REFERENCES "agent" ("id") ON DELETE CASCADE,
    "enabled" BOOLEAN,
    "config" JSONB,
    "credential_refs" JSONB NOT NULL DEFAULT '{}',
    "revision" BIGINT NOT NULL DEFAULT 1,
    "created_at" TIMESTAMPTZ NOT NULL DEFAULT now(),
    "updated_at" TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT "plugin_config_definition_fkey"
        FOREIGN KEY ("plugin_id", "namespace")
        REFERENCES "plugin_definition" ("id", "namespace") ON DELETE RESTRICT,
    CONSTRAINT "plugin_config_scope_owner_check" CHECK (
        ("scope" = 'system' AND "user_id" IS NULL AND "agent_id" IS NULL)
        OR ("scope" = 'system_agent' AND "user_id" IS NULL AND "agent_id" IS NOT NULL)
        OR ("scope" = 'user' AND "user_id" IS NOT NULL AND "agent_id" IS NULL)
        OR ("scope" = 'user_agent' AND "user_id" IS NOT NULL AND "agent_id" IS NOT NULL)
    ),
    CONSTRAINT "plugin_config_revision_check" CHECK ("revision" > 0),
    CONSTRAINT "plugin_config_config_object_check" CHECK ("config" IS NULL OR jsonb_typeof("config") = 'object'),
    CONSTRAINT "plugin_config_credential_refs_object_check" CHECK (jsonb_typeof("credential_refs") = 'object'),
    CONSTRAINT "plugin_config_negative_check" CHECK ("config" IS NOT NULL OR "enabled" IS FALSE),
    CONSTRAINT "plugin_config_negative_refs_check" CHECK ("config" IS NOT NULL OR "credential_refs" = '{}'::jsonb)
);
CREATE UNIQUE INDEX "uniq_plugin_config_owner" ON "plugin_config"
    ("plugin_id", "scope", "user_id", "agent_id") NULLS NOT DISTINCT;
CREATE UNIQUE INDEX "uniq_plugin_config_namespace_payload" ON "plugin_config"
    ("namespace", "scope", "user_id", "agent_id") NULLS NOT DISTINCT
    WHERE "config" IS NOT NULL;
CREATE INDEX "idx_plugin_config_user" ON "plugin_config" ("user_id");
CREATE INDEX "idx_plugin_config_agent" ON "plugin_config" ("agent_id");

-- Add identity columns before the runtime cutover; legacy tool_name writes
-- continue unchanged until the later tool-policy migration.
ALTER TABLE tool_override
    ADD COLUMN plugin_id TEXT REFERENCES plugin_definition(id) ON DELETE RESTRICT,
    ADD COLUMN local_tool_name TEXT,
    ADD CONSTRAINT tool_override_plugin_identity_pair CHECK (
        (plugin_id IS NULL AND local_tool_name IS NULL)
        OR (plugin_id IS NOT NULL AND local_tool_name IS NOT NULL AND local_tool_name <> '')
    );
CREATE INDEX idx_tool_override_plugin_id ON tool_override(plugin_id);

-- +goose Down
ALTER TABLE tool_override DROP COLUMN local_tool_name, DROP COLUMN plugin_id;

DROP TABLE "plugin_config";
DROP TABLE "plugin_definition";
