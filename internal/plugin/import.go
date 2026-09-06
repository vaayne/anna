package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CherryHQ/stella/pkg/db/sqlc"
	"github.com/CherryHQ/stella/pkg/toolmeta"
)

const (
	legacyMCPTransportHTTP = "streamable_http"
	legacyMCPTransportSSE  = "sse"
	legacyMCPAuthNone      = "none"
	legacyMCPAuthBearer    = "bearer"
	legacyMCPAuthOAuth     = "oauth"
	legacyMCPCredShared    = "shared"
	legacyMCPCredPerUser   = "per_user"
)

const cutoverMarkerKey = "plugin_cutover_v1"

var (
	ErrImportComplete          = errors.New("plugin: legacy state was already imported")
	ErrToolOverrideSchema      = errors.New("plugin: tool override cutover identity constraints are not ready")
	ErrOAuthForeignKeySchema   = errors.New("plugin: MCP OAuth flow foreign key cutover is not ready")
	ErrLegacyPluginUnknown     = errors.New("plugin: legacy plugin has no trusted definition")
	ErrLegacyMigrationConflict = errors.New("plugin: legacy state cannot be imported without losing data")
)

// LegacyPlugin is the old generic plugin row. Config is an opaque JSON object;
// the importer never treats it as a definition when a trusted catalog entry
// exists.
type LegacyPlugin struct {
	ID      string
	Kind    string
	Name    string
	Enabled bool
	Config  json.RawMessage
}

// LegacyChannel is the identity-only snapshot of one old channel instance.
// Its configuration stays in the legacy channel row and is never copied into
// a plugin config during this import.
type LegacyChannel struct {
	ID   string
	Type string
}

// LegacyManifestOverride is the old manifest override row. Config may be a
// sparse patch ($sparse=true) or a pre-sparse full snapshot.
type LegacyManifestOverride struct {
	PluginID           string
	Enabled            *bool
	SessionEnvVaultKey string
	Config             string
}

// LegacyMCPRegistration is a secret-free snapshot of one mcp_server row.
// CredentialRef and OAuth references are locators only; secret values never
// enter this type or the target JSON.
type LegacyMCPRegistration struct {
	ID                      string
	Scope                   string
	UserID                  string
	AgentID                 string
	Name                    string
	URL                     string
	Transport               string
	AuthType                string
	CredentialRef           string
	Enabled                 bool
	Metadata                map[string]any
	Status                  string
	StatusError             string
	Tools                   json.RawMessage
	CredentialMode          string
	OAuthClientSecretExists bool
}

// LegacyToolOverride is retained for the conversion helper. The target schema
// needs plugin_id/local_tool_name before an MCP row can be migrated safely.
type LegacyToolOverride struct {
	ID       string
	ToolName string
	Scope    string
	UserID   string
	AgentID  string
	Enabled  bool
}

// LegacySnapshot is read while the cutover transaction holds the advisory
// lock. It is also useful to normalize an offline snapshot for a maintenance
// preview without touching a database.
type LegacySnapshot struct {
	Plugins           []LegacyPlugin
	Channels          []LegacyChannel
	ManifestOverrides []LegacyManifestOverride
	MCP               []LegacyMCPRegistration
	ToolOverrides     []LegacyToolOverride
}

// ToolOverrideMigration identifies one exact target row. The legacy exported
// name is retained for audit/debugging, while the target identity is the
// trusted plugin/local pair.
type ToolOverrideMigration struct {
	LegacyID  string
	OldName   string
	NewName   string
	PluginID  string
	ConfigID  string
	Namespace string
	LocalTool string
	Scope     Scope
	UserID    string
	AgentID   string
	Enabled   bool
}

// ImportPlan is the fully validated additive write set. Callers should log
// only its IDs and counts; payloads can contain private endpoints/metadata.
type ImportPlan struct {
	Definitions   []Definition
	Configs       []Config
	ToolOverrides []ToolOverrideMigration
	CoreOverrides []LegacyToolOverride
}

// NormalizeLegacySnapshot converts all supported old state into the new
// definition/config model. It does not mutate a catalog or a database. Any
// ambiguity is returned before a caller can write a marker.
func NormalizeLegacySnapshot(snapshot LegacySnapshot, catalog *Catalog, metadata *toolmeta.Registry) (ImportPlan, error) {
	if catalog == nil {
		return ImportPlan{}, ErrInvalidDefinition
	}

	defs := make(map[string]Definition)
	for _, def := range catalog.Definitions() {
		if err := def.Validate(); err != nil {
			return ImportPlan{}, fmt.Errorf("catalog %s: %w", def.ID, err)
		}
		defs[def.ID] = def
	}

	overrides := make(map[string]LegacyManifestOverride, len(snapshot.ManifestOverrides))
	legacyIDs := make(map[string]string)
	configIDs := make(map[string]string)
	for _, override := range snapshot.ManifestOverrides {
		if _, exists := overrides[override.PluginID]; exists {
			return ImportPlan{}, fmt.Errorf("%w: duplicate manifest override %q", ErrLegacyMigrationConflict, override.PluginID)
		}
		overrides[override.PluginID] = override
		if _, trusted := defs[override.PluginID]; trusted || override.Config == "" {
			continue
		}
		newID := legacyCustomDefinitionID(override.PluginID)
		def, err := customDefinitionFromManifest(newID, override.Config)
		if err != nil {
			return ImportPlan{}, err
		}
		if err := def.Validate(); err != nil {
			return ImportPlan{}, fmt.Errorf("legacy manifest %s: %w", override.PluginID, err)
		}
		defs[def.ID] = def
		legacyIDs[override.PluginID] = def.ID
		configIDs[def.ID] = strings.TrimPrefix(def.ID, "custom/")
	}

	configs := make(map[string]ConfigAccumulator)
	for _, legacy := range snapshot.Plugins {
		id := legacyIDs[legacyPluginID(legacy)]
		if id == "" {
			id = legacyPluginID(legacy)
		}
		def, ok := defs[id]
		if !ok {
			return ImportPlan{}, fmt.Errorf("%w: %s", ErrLegacyPluginUnknown, id)
		}
		acc := configs[id]
		if err := acc.addLegacyPlugin(def, legacy, snapshot.Channels); err != nil {
			return ImportPlan{}, err
		}
		configs[id] = acc
	}

	for _, override := range snapshot.ManifestOverrides {
		pluginID := legacyIDs[override.PluginID]
		if pluginID == "" {
			pluginID = override.PluginID
		}
		def, ok := defs[pluginID]
		if !ok {
			return ImportPlan{}, fmt.Errorf("%w: %s", ErrLegacyPluginUnknown, override.PluginID)
		}
		acc := configs[pluginID]
		if err := acc.addManifestOverride(def, override, legacyIDs[override.PluginID] != ""); err != nil {
			return ImportPlan{}, err
		}
		configs[pluginID] = acc
	}

	for _, legacy := range snapshot.MCP {
		def, config, err := normalizeMCP(legacy)
		if err != nil {
			return ImportPlan{}, err
		}
		if err := def.Validate(); err != nil {
			return ImportPlan{}, fmt.Errorf("MCP %s: %w", legacy.ID, err)
		}
		if existing, exists := defs[def.ID]; exists {
			if existing.Namespace != def.Namespace || existing.Backend != def.Backend || existing.ImplementationKey != def.ImplementationKey {
				return ImportPlan{}, fmt.Errorf("%w: MCP definition %s", ErrLegacyMigrationConflict, def.ID)
			}
		} else {
			defs[def.ID] = def
		}
		configs[def.ID] = ConfigAccumulator{config: config, hasConfig: true}
	}

	plan := ImportPlan{Definitions: make([]Definition, 0, len(defs)), Configs: make([]Config, 0, len(configs))}
	for _, def := range defs {
		plan.Definitions = append(plan.Definitions, def)
	}
	for id, acc := range configs {
		if !acc.hasConfig {
			continue
		}
		config := acc.config
		if len(config.Payload) == 0 && config.Enabled == nil && len(config.CredentialRefs) == 0 {
			continue
		}
		if len(config.Payload) == 0 && config.Enabled != nil && *config.Enabled {
			config.Payload = json.RawMessage(`{}`)
		}
		if config.ID == "" {
			config.ID = configIDs[id]
			if config.ID == "" {
				config.ID = stableLegacyConfigID(id)
			}
		}
		if err := config.Validate(); err != nil {
			return ImportPlan{}, fmt.Errorf("legacy config %s: %w", id, err)
		}
		plan.Configs = append(plan.Configs, config)
	}
	if err := validateNamespaceOwners(plan.Configs, plan.Definitions); err != nil {
		return ImportPlan{}, err
	}

	seenToolOverrides := make(map[string]string, len(snapshot.ToolOverrides))
	for _, override := range snapshot.ToolOverrides {
		migration, err := ConvertLegacyToolOverride(override, snapshot.MCP, metadata)
		if err != nil {
			return ImportPlan{}, err
		}
		if migration.PluginID != "" {
			def, ok := defs[migration.PluginID]
			if !ok {
				return ImportPlan{}, fmt.Errorf("%w: tool override %s references unknown plugin %s", ErrLegacyMigrationConflict, override.ID, migration.PluginID)
			}
			if migration.Namespace != def.Namespace {
				return ImportPlan{}, fmt.Errorf("%w: tool override %s namespace %q does not match plugin %s namespace %q", ErrLegacyMigrationConflict, override.ID, migration.Namespace, migration.PluginID, def.Namespace)
			}
			if migration.ConfigID != "" {
				config, found := findConfigByID(plan.Configs, migration.ConfigID)
				if !found || config.PluginID != migration.PluginID || config.Namespace != migration.Namespace {
					return ImportPlan{}, fmt.Errorf("%w: tool override %s references mismatched config %s", ErrLegacyMigrationConflict, override.ID, migration.ConfigID)
				}
			}
			key := strings.Join([]string{migration.PluginID, migration.LocalTool, string(migration.Scope), migration.UserID, migration.AgentID}, "\x00")
			if prior, exists := seenToolOverrides[key]; exists {
				return ImportPlan{}, fmt.Errorf("%w: tool overrides %s and %s target the same identity", ErrLegacyMigrationConflict, prior, override.ID)
			}
			seenToolOverrides[key] = override.ID
			plan.ToolOverrides = append(plan.ToolOverrides, migration)
		} else {
			plan.CoreOverrides = append(plan.CoreOverrides, override)
		}
	}
	slices.SortFunc(plan.Definitions, func(a, b Definition) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(plan.Configs, func(a, b Config) int { return strings.Compare(a.PluginID, b.PluginID) })
	slices.SortFunc(plan.ToolOverrides, func(a, b ToolOverrideMigration) int { return strings.Compare(a.LegacyID, b.LegacyID) })
	slices.SortFunc(plan.CoreOverrides, func(a, b LegacyToolOverride) int { return strings.Compare(a.ID, b.ID) })
	return plan, nil
}

// PreviewLegacyImport reads and normalizes legacy state under the same
// advisory transaction lock that the eventual cutover will use. It deliberately
// performs no writes, including the durable marker. The final importer belongs
// to the cutover phase after all target identity columns exist.
func PreviewLegacyImport(ctx context.Context, db *pgxpool.Pool, catalog *Catalog, metadata *toolmeta.Registry) (ImportPlan, error) {
	if db == nil || catalog == nil {
		return ImportPlan{}, ErrInvalidDefinition
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return ImportPlan{}, fmt.Errorf("begin plugin cutover preview: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := sqlc.New(tx).LockPluginCatalog(ctx); err != nil {
		return ImportPlan{}, fmt.Errorf("lock plugin cutover preview: %w", err)
	}
	var marker string
	err = tx.QueryRow(ctx, `SELECT value FROM app_setting WHERE key = $1`, cutoverMarkerKey).Scan(&marker)
	if err == nil {
		if marker == "v1" {
			return ImportPlan{}, ErrImportComplete
		}
		return ImportPlan{}, fmt.Errorf("%w: unexpected cutover marker", ErrLegacyMigrationConflict)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ImportPlan{}, fmt.Errorf("read plugin cutover marker: %w", err)
	}

	snapshot, err := readLegacySnapshot(ctx, tx)
	if err != nil {
		return ImportPlan{}, err
	}
	plan, err := NormalizeLegacySnapshot(snapshot, catalog, metadata)
	if err != nil {
		return plan, err
	}
	return plan, nil
}

// ImportLegacyState performs the one-time legacy cutover. The startup caller
// must stop every old stellad replica that can write this database before
// invoking it. The advisory lock serializes new callers, but cannot fence an
// old binary. All target writes and the marker share one transaction.
//
// Tool-policy rows are written only after their trusted identity mapping has
// been validated. The MCP OAuth FK retarget remains a schema prerequisite.
func ImportLegacyState(ctx context.Context, db *pgxpool.Pool, catalog *Catalog, metadata *toolmeta.Registry) error {
	if db == nil || catalog == nil {
		return ErrInvalidDefinition
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin plugin cutover: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return fmt.Errorf("set plugin cutover lock timeout: %w", err)
	}
	if err := sqlc.New(tx).LockPluginCatalog(ctx); err != nil {
		return fmt.Errorf("lock plugin cutover: %w", err)
	}
	var marker string
	err = tx.QueryRow(ctx, `SELECT value FROM app_setting WHERE key = $1`, cutoverMarkerKey).Scan(&marker)
	if err == nil {
		if marker == "v1" {
			return ErrImportComplete
		}
		return fmt.Errorf("%w: unexpected cutover marker", ErrLegacyMigrationConflict)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read plugin cutover marker: %w", err)
	}

	snapshot, err := readLegacySnapshot(ctx, tx)
	if err != nil {
		return err
	}
	plan, err := NormalizeLegacySnapshot(snapshot, catalog, metadata)
	if err != nil {
		return err
	}
	if err := legacyImportSchemaReady(ctx, tx); err != nil {
		return err
	}
	if err := verifyCoreOverrides(ctx, tx, plan.CoreOverrides, metadata); err != nil {
		return err
	}

	q := sqlc.New(tx)
	for _, def := range plan.Definitions {
		if _, err := q.UpsertPluginDefinition(ctx, sqlc.UpsertPluginDefinitionParams{
			ID: def.ID, Namespace: def.Namespace, DisplayName: def.DisplayName,
			Backend: string(def.Backend), Source: string(def.Source),
			ImplementationKey: def.ImplementationKey, Spec: def.Spec,
			DefaultEnabled: def.DefaultEnabled, Revision: def.Revision,
			CreatorUserID: nullableText(def.CreatorUserID),
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: definition %s identity conflicts with target row", ErrLegacyMigrationConflict, def.ID)
			}
			return fmt.Errorf("import plugin definition %s: %w", def.ID, err)
		}
	}

	configs := make(map[string]Config, len(plan.Configs))
	for _, config := range plan.Configs {
		if _, err := q.CreatePluginConfig(ctx, sqlc.CreatePluginConfigParams{
			ID: config.ID, PluginID: config.PluginID, Namespace: config.Namespace,
			Scope: string(config.Scope), UserID: nullableText(config.UserID),
			AgentID: nullableText(config.AgentID), Enabled: nullableBool(config.Enabled),
			Config: config.Payload, CredentialRefs: nonEmptyJSON(config.CredentialRefs),
			Revision: config.Revision,
		}); err != nil {
			return fmt.Errorf("import plugin config %s: %w", config.PluginID, mapConflict(err))
		}
		configs[config.ID] = config
	}
	if err := importToolOverrides(ctx, tx, plan); err != nil {
		return err
	}

	// Preserve a concrete system projection for every shipped definition. An
	// old system row, including an explicit false or a pinned payload, wins via
	// ON CONFLICT DO NOTHING; missing rows inherit the shipped default.
	for _, def := range catalog.Definitions() {
		if def.Source != SourceBuiltin {
			continue
		}
		if _, err := q.EnsureSystemPluginConfig(ctx, sqlc.EnsureSystemPluginConfigParams{
			PluginID: def.ID, Namespace: def.Namespace,
		}); err != nil {
			return fmt.Errorf("ensure builtin plugin config %s: %w", def.ID, err)
		}
	}

	for _, registration := range snapshot.MCP {
		if effectiveCredentialMode(registration.CredentialMode) != legacyMCPCredShared {
			// A per-user legacy tools cache has no proven probing owner. Keep it
			// cold in the old row and let each user probe after cutover.
			continue
		}
		config, ok := configs[registration.ID]
		if !ok {
			return fmt.Errorf("%w: MCP %s has no imported config", ErrLegacyMigrationConflict, registration.ID)
		}
		tools := registration.Tools
		if len(tools) == 0 || bytes.Equal(bytes.TrimSpace(tools), []byte("null")) {
			tools = json.RawMessage(`[]`)
		}
		status := registration.Status
		if status == "" {
			status = "unknown"
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO mcp_connection_state (
				config_id, credential_user_id, tools, status, status_error,
				probed_at, config_revision
			) VALUES ($1::uuid, NULL, $2::jsonb, $3, $4, NULL, $5)
			ON CONFLICT (config_id, credential_user_id) DO UPDATE SET
				tools = EXCLUDED.tools, status = EXCLUDED.status,
				status_error = EXCLUDED.status_error, probed_at = EXCLUDED.probed_at,
				config_revision = EXCLUDED.config_revision, updated_at = now()
		`, registration.ID, tools, status, registration.StatusError, config.Revision); err != nil {
			return fmt.Errorf("import MCP observation %s: %w", registration.ID, err)
		}
	}
	if err := validateLegacyOAuthForeignKey(ctx, tx); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO app_setting (key, value, updated_at)
		VALUES ($1, 'v1', now())
	`, cutoverMarkerKey); err != nil {
		return fmt.Errorf("write plugin cutover marker: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit plugin cutover: %w", err)
	}
	return nil
}

// legacyImportSchemaReady checks the final-cutover-only schema edges without
// changing them. In particular, mcp_oauth_flow.server_id must already point at
// the UUID-preserving plugin_config row before a marker can make the new
// runtime authoritative. This keeps preparation migrations from being
// mistaken for a complete cutover.
func legacyImportSchemaReady(ctx context.Context, tx pgx.Tx) error {
	var migration41Applied, pluginIdentity, localToolIdentity, observationTable, oauthConfigFK, toolNameNullable, identityCheck, coreIndex, pluginIndex bool
	err := tx.QueryRow(ctx, `
		SELECT
			COALESCE((
				SELECT is_applied
				FROM goose_db_version
				WHERE version_id = 90000000000041
				ORDER BY id DESC LIMIT 1
			), false),
			to_regclass('public.mcp_connection_state') IS NOT NULL,
			EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = 'public' AND table_name = 'tool_override'
				  AND column_name = 'plugin_id'
			),
			EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = 'public' AND table_name = 'tool_override'
				  AND column_name = 'local_tool_name'
			),
			EXISTS (
				SELECT 1
				FROM pg_constraint c
				JOIN pg_class child ON child.oid = c.conrelid
				JOIN pg_class parent ON parent.oid = c.confrelid
				JOIN pg_attribute child_col ON child_col.attrelid = child.oid
				  AND child_col.attnum = c.conkey[1]
				JOIN pg_attribute parent_col ON parent_col.attrelid = parent.oid
				  AND parent_col.attnum = c.confkey[1]
				WHERE c.contype = 'f'
				  AND c.conname = 'mcp_oauth_flow_server_id_plugin_config_fkey'
				  AND child.relnamespace = 'public'::regnamespace
				  AND child.relname = 'mcp_oauth_flow'
				  AND parent.relname = 'plugin_config'
				  AND parent.relnamespace = 'public'::regnamespace
				  AND child_col.attname = 'server_id'
				  AND parent_col.attname = 'id'
				  AND c.confdeltype = 'c'
				  AND cardinality(c.conkey) = 1
				  AND cardinality(c.confkey) = 1
				  AND (
					  SELECT count(*)
					  FROM pg_constraint extra
					  WHERE extra.contype = 'f'
						AND extra.conrelid = child.oid
						AND cardinality(extra.conkey) = 1
						AND extra.conkey[1] = child_col.attnum
				  ) = 1
			),
			EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = 'public' AND table_name = 'tool_override'
				  AND column_name = 'tool_name' AND is_nullable = 'YES'
			),
			EXISTS (
				SELECT 1 FROM pg_constraint c
				JOIN pg_class table_ref ON table_ref.oid = c.conrelid
				WHERE c.contype = 'c' AND c.conname = 'tool_override_identity_check'
				  AND table_ref.relname = 'tool_override' AND c.convalidated
			),
			EXISTS (
				SELECT 1 FROM pg_class table_ref
				JOIN pg_index index_ref ON index_ref.indrelid = table_ref.oid
				WHERE table_ref.relname = 'tool_override'
				  AND index_ref.indexrelid = to_regclass('public.uniq_tool_override_core_identity')
				  AND index_ref.indisunique
			),
			EXISTS (
				SELECT 1 FROM pg_class table_ref
				JOIN pg_index index_ref ON index_ref.indrelid = table_ref.oid
				WHERE table_ref.relname = 'tool_override'
				  AND index_ref.indexrelid = to_regclass('public.uniq_tool_override_plugin_identity')
				  AND index_ref.indisunique
			)
	`).Scan(&migration41Applied, &observationTable, &pluginIdentity, &localToolIdentity, &oauthConfigFK, &toolNameNullable, &identityCheck, &coreIndex, &pluginIndex)
	if err != nil {
		return fmt.Errorf("check plugin cutover schema: %w", err)
	}
	if !migration41Applied || !pluginIdentity || !localToolIdentity {
		return ErrToolOverrideSchema
	}
	if !toolNameNullable || !identityCheck || !coreIndex || !pluginIndex {
		return ErrToolOverrideSchema
	}
	if !observationTable {
		return fmt.Errorf("%w: mcp_connection_state table is missing", ErrLegacyMigrationConflict)
	}
	if !oauthConfigFK {
		return ErrOAuthForeignKeySchema
	}
	return nil
}

// verifyCoreOverrides proves that every legacy row left in the core identity
// space is both a trusted current core name and the same source row read by the
// import plan. Unknown names must not silently fall through to core: a missing
// plugin metadata entry is a mapping failure, not evidence of core ownership.
func verifyCoreOverrides(ctx context.Context, tx pgx.Tx, overrides []LegacyToolOverride, metadata *toolmeta.Registry) error {
	if len(overrides) == 0 {
		return nil
	}
	if metadata == nil {
		return fmt.Errorf("%w: core tool metadata registry is required", ErrToolOverrideSchema)
	}
	for _, override := range overrides {
		if override.ToolName == "" || !validLegacyScope(override.Scope) || !legacyOwnerMatches(override.Scope, override.UserID, override.AgentID) {
			return fmt.Errorf("%w: invalid core override identity %s", ErrLegacyMigrationConflict, override.ID)
		}
		spec, known := metadata.Lookup(override.ToolName)
		if known {
			if spec.Name != override.ToolName || spec.PluginID != "" || spec.Namespace != "" || spec.LocalName != "" {
				return fmt.Errorf("%w: legacy override %s is not a trusted core tool", ErrLegacyMigrationConflict, override.ToolName)
			}
		} else if !toolmeta.HandWritten(override.ToolName) {
			for _, candidate := range metadata.Tools() {
				if candidate.PluginID != "" && candidate.Family != "" && candidate.LocalName != "" && candidate.Family+"_"+candidate.LocalName == override.ToolName {
					return fmt.Errorf("%w: legacy plugin tool %s was not mapped", ErrLegacyMigrationConflict, override.ToolName)
				}
			}
			return fmt.Errorf("%w: unknown legacy core tool %s", ErrLegacyMigrationConflict, override.ToolName)
		}
		if _, err := uuid.Parse(override.ID); err != nil {
			return fmt.Errorf("%w: core override %s has invalid row ID", ErrLegacyMigrationConflict, override.ID)
		}
		var storedToolName, storedScope, storedUserID, storedAgentID, storedPluginID, storedLocalToolName pgtype.Text
		var storedEnabled bool
		err := tx.QueryRow(ctx, `
			SELECT tool_name, scope, user_id::text, agent_id, enabled, plugin_id, local_tool_name
			FROM tool_override
			WHERE id = $1::uuid
		`, override.ID).Scan(&storedToolName, &storedScope, &storedUserID, &storedAgentID, &storedEnabled, &storedPluginID, &storedLocalToolName)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: core override %s source row disappeared", ErrLegacyMigrationConflict, override.ID)
		}
		if err != nil {
			return fmt.Errorf("verify core override %s: %w", override.ID, err)
		}
		if storedPluginID.Valid || storedLocalToolName.Valid || !storedToolName.Valid || storedToolName.String != override.ToolName || !storedScope.Valid || storedScope.String != override.Scope ||
			textValue(storedUserID) != override.UserID || textValue(storedAgentID) != override.AgentID || storedEnabled != override.Enabled {
			return fmt.Errorf("%w: core override %s changed since snapshot", ErrLegacyMigrationConflict, override.ID)
		}
	}
	return nil
}

func importToolOverrides(ctx context.Context, tx pgx.Tx, plan ImportPlan) error {
	seen := make(map[string]string, len(plan.ToolOverrides)+len(plan.CoreOverrides))
	for _, override := range plan.CoreOverrides {
		if override.ToolName == "" {
			return fmt.Errorf("%w: incomplete core identity for legacy override %s", ErrLegacyMigrationConflict, override.ID)
		}
		key := legacyOverrideTargetKey("core", override.ToolName, override.Scope, override.UserID, override.AgentID)
		if prior, exists := seen[key]; exists {
			return fmt.Errorf("%w: tool overrides %s and %s target the same identity", ErrLegacyMigrationConflict, prior, override.ID)
		}
		seen[key] = override.ID
		// Core rows already live in tool_override. They retain their durable
		// tool_name and need no second row during the same-table cutover.
	}
	for _, override := range plan.ToolOverrides {
		if override.PluginID == "" || override.LocalTool == "" || override.NewName == "" {
			return fmt.Errorf("%w: incomplete target identity for legacy override %s", ErrLegacyMigrationConflict, override.LegacyID)
		}
		key := legacyOverrideTargetKey(override.PluginID, override.LocalTool, string(override.Scope), override.UserID, override.AgentID)
		if prior, exists := seen[key]; exists {
			return fmt.Errorf("%w: tool overrides %s and %s target the same identity", ErrLegacyMigrationConflict, prior, override.LegacyID)
		}
		seen[key] = override.LegacyID
		// The source and target policy share this table. Update the source row
		// in place so its UUID, owner, scope, enabled value, and audit timestamps
		// survive while the durable identity changes from the exported name to
		// the trusted plugin/local pair. Requiring the old row prevents a plan
		// from silently creating policy detached from the snapshot it validated.
		var updatedID string
		err := tx.QueryRow(ctx, `
			UPDATE tool_override
			SET tool_name = NULL, plugin_id = $2, local_tool_name = $3, updated_at = now()
			WHERE id = $1::uuid
			  AND tool_name IS NOT DISTINCT FROM $4::text
			  AND scope IS NOT DISTINCT FROM $5::text
			  AND user_id::text IS NOT DISTINCT FROM NULLIF($6::text, '')
			  AND agent_id IS NOT DISTINCT FROM NULLIF($7::text, '')
			  AND enabled IS NOT DISTINCT FROM $8::boolean
			  AND plugin_id IS NULL AND local_tool_name IS NULL
			RETURNING id::text
		`, override.LegacyID, override.PluginID, override.LocalTool, override.OldName, string(override.Scope), override.UserID, override.AgentID, override.Enabled).Scan(&updatedID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: source tool override %s disappeared or was already migrated", ErrLegacyMigrationConflict, override.LegacyID)
		}
		if err != nil {
			return fmt.Errorf("import tool override %s (%s): %w", override.LegacyID, override.NewName, err)
		}
	}
	return nil
}

func legacyOverrideTargetKey(owner, local, scope, userID, agentID string) string {
	return strings.Join([]string{owner, local, scope, userID, agentID}, "\x00")
}

// validateLegacyOAuthForeignKey is intentionally called after config writes:
// the final migration installs this FK as NOT VALID so existing flow rows can
// continue to reference the UUID-preserving target config during the one
// maintenance transaction. Validation is the last schema gate before marker.
func validateLegacyOAuthForeignKey(ctx context.Context, tx pgx.Tx) error {
	const constraintName = "mcp_oauth_flow_server_id_plugin_config_fkey"
	var found bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_constraint c
			JOIN pg_class child ON child.oid = c.conrelid
			JOIN pg_class parent ON parent.oid = c.confrelid
			JOIN pg_attribute child_col ON child_col.attrelid = child.oid
			  AND child_col.attnum = c.conkey[1]
			JOIN pg_attribute parent_col ON parent_col.attrelid = parent.oid
			  AND parent_col.attnum = c.confkey[1]
			WHERE c.contype = 'f'
			  AND c.conname = $1
			  AND child.relnamespace = 'public'::regnamespace
			  AND child.relname = 'mcp_oauth_flow'
			  AND parent.relnamespace = 'public'::regnamespace
			  AND parent.relname = 'plugin_config'
			  AND child_col.attname = 'server_id'
			  AND parent_col.attname = 'id'
			  AND c.confdeltype = 'c'
			  AND cardinality(c.conkey) = 1
			  AND cardinality(c.confkey) = 1
			  AND (
				  SELECT count(*)
				  FROM pg_constraint extra
				  WHERE extra.contype = 'f'
					AND extra.conrelid = child.oid
					AND cardinality(extra.conkey) = 1
					AND extra.conkey[1] = child_col.attnum
			  ) = 1
		)
	`, constraintName).Scan(&found)
	if err != nil {
		return fmt.Errorf("find plugin OAuth foreign key: %w", err)
	}
	if !found {
		return ErrOAuthForeignKeySchema
	}
	quoted := `"` + constraintName + `"`
	if _, err := tx.Exec(ctx, `ALTER TABLE public.mcp_oauth_flow VALIDATE CONSTRAINT `+quoted); err != nil {
		return fmt.Errorf("validate plugin OAuth foreign key: %w", err)
	}
	return nil
}

func readLegacySnapshot(ctx context.Context, tx pgx.Tx) (LegacySnapshot, error) {
	var snapshot LegacySnapshot
	rows, err := tx.Query(ctx, `SELECT id, kind, name, enabled, config FROM plugin ORDER BY id`)
	if err != nil {
		return LegacySnapshot{}, fmt.Errorf("read legacy plugins: %w", err)
	}
	for rows.Next() {
		var row LegacyPlugin
		if err := rows.Scan(&row.ID, &row.Kind, &row.Name, &row.Enabled, &row.Config); err != nil {
			rows.Close()
			return LegacySnapshot{}, fmt.Errorf("scan legacy plugin: %w", err)
		}
		snapshot.Plugins = append(snapshot.Plugins, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return LegacySnapshot{}, fmt.Errorf("read legacy plugins: %w", err)
	}
	rows.Close()

	rows, err = tx.Query(ctx, `SELECT id, type FROM channel ORDER BY id`)
	if err != nil {
		return LegacySnapshot{}, fmt.Errorf("read legacy channels: %w", err)
	}
	for rows.Next() {
		var row LegacyChannel
		if err := rows.Scan(&row.ID, &row.Type); err != nil {
			rows.Close()
			return LegacySnapshot{}, fmt.Errorf("scan legacy channel: %w", err)
		}
		snapshot.Channels = append(snapshot.Channels, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return LegacySnapshot{}, fmt.Errorf("read legacy channels: %w", err)
	}
	rows.Close()

	rows, err = tx.Query(ctx, `SELECT plugin_id, enabled, session_env_vault_key, config FROM plugin_override ORDER BY plugin_id`)
	if err != nil {
		return LegacySnapshot{}, fmt.Errorf("read legacy manifest overrides: %w", err)
	}
	for rows.Next() {
		var row LegacyManifestOverride
		var enabled pgtype.Bool
		if err := rows.Scan(&row.PluginID, &enabled, &row.SessionEnvVaultKey, &row.Config); err != nil {
			rows.Close()
			return LegacySnapshot{}, fmt.Errorf("scan legacy manifest override: %w", err)
		}
		if enabled.Valid {
			row.Enabled = &enabled.Bool
		}
		snapshot.ManifestOverrides = append(snapshot.ManifestOverrides, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return LegacySnapshot{}, fmt.Errorf("read legacy manifest overrides: %w", err)
	}
	rows.Close()

	rows, err = tx.Query(ctx, `
		SELECT id, scope, user_id, agent_id, name, url, transport, auth_type,
		       credential_ref, enabled, metadata, status, status_error, tools, credential_mode
		FROM mcp_server ORDER BY id
	`)
	if err != nil {
		return LegacySnapshot{}, fmt.Errorf("read legacy MCP registrations: %w", err)
	}
	for rows.Next() {
		var row LegacyMCPRegistration
		var userID, agentID pgtype.Text
		if err := rows.Scan(&row.ID, &row.Scope, &userID, &agentID, &row.Name, &row.URL, &row.Transport, &row.AuthType,
			&row.CredentialRef, &row.Enabled, &row.Metadata, &row.Status, &row.StatusError, &row.Tools, &row.CredentialMode); err != nil {
			rows.Close()
			return LegacySnapshot{}, fmt.Errorf("scan legacy MCP registration: %w", err)
		}
		row.UserID, row.AgentID = textValue(userID), textValue(agentID)
		snapshot.MCP = append(snapshot.MCP, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return LegacySnapshot{}, fmt.Errorf("read legacy MCP registrations: %w", err)
	}
	rows.Close()
	for index := range snapshot.MCP {
		row := &snapshot.MCP[index]
		if row.AuthType != legacyMCPAuthOAuth {
			continue
		}
		row.OAuthClientSecretExists, err = legacyVaultEntryExists(ctx, tx, row.Scope, row.UserID, row.AgentID, legacyOAuthClientSecretName(row.ID))
		if err != nil {
			return LegacySnapshot{}, fmt.Errorf("read legacy MCP OAuth credential presence: %w", err)
		}
	}

	rows, err = tx.Query(ctx, `SELECT id, tool_name, scope, user_id, agent_id, enabled FROM tool_override ORDER BY id`)
	if err != nil {
		return LegacySnapshot{}, fmt.Errorf("read legacy tool overrides: %w", err)
	}
	for rows.Next() {
		var row LegacyToolOverride
		var userID, agentID pgtype.Text
		if err := rows.Scan(&row.ID, &row.ToolName, &row.Scope, &userID, &agentID, &row.Enabled); err != nil {
			rows.Close()
			return LegacySnapshot{}, fmt.Errorf("scan legacy tool override: %w", err)
		}
		row.UserID, row.AgentID = textValue(userID), textValue(agentID)
		snapshot.ToolOverrides = append(snapshot.ToolOverrides, row)
	}
	if err := rows.Err(); err != nil {
		return LegacySnapshot{}, fmt.Errorf("read legacy tool overrides: %w", err)
	}
	return snapshot, nil
}

// legacyVaultEntryExists deliberately selects only the existence bit. Legacy
// migration must prove that a derived secret locator has backing material
// without loading or decrypting ciphertext into the importer process.
func legacyVaultEntryExists(ctx context.Context, tx pgx.Tx, scope, userID, agentID, name string) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM vault_entry
			WHERE scope = $1
			  AND coalesce(user_id::text, '') = coalesce($2::text, '')
			  AND coalesce(agent_id, '') = coalesce($3::text, '')
			  AND name = $4
		)
	`, scope, userID, agentID, name).Scan(&exists)
	return exists, err
}

func legacyPluginID(row LegacyPlugin) string {
	if row.ID != "" {
		return row.ID
	}
	return row.Kind + "/" + row.Name
}

func stableLegacyConfigID(pluginID string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("stella://plugin-config/"+pluginID)).String()
}

func legacyCustomDefinitionID(oldID string) string {
	return "custom/" + stableLegacyConfigID("legacy-definition/"+oldID)
}

type namespaceOwner struct {
	namespace string
	scope     Scope
	userID    string
	agentID   string
}

func validateNamespaceOwners(configs []Config, definitions []Definition) error {
	claims := slices.Clone(configs)
	systemOverrides := make(map[string]bool)
	for _, config := range configs {
		if config.Scope == ScopeSystem {
			systemOverrides[config.PluginID] = true
		}
	}
	// Missing builtin rows will acquire a payload-bearing system projection.
	// These are conflict-check claims only, never fabricated persistent UUIDs.
	for _, def := range definitions {
		if def.Source == SourceBuiltin && !systemOverrides[def.ID] {
			claims = append(claims, Config{PluginID: def.ID, Namespace: def.Namespace, Scope: ScopeSystem, Payload: json.RawMessage(`{}`)})
		}
	}
	owners := make(map[namespaceOwner]string, len(claims))
	for _, config := range claims {
		if len(config.Payload) == 0 {
			continue
		}
		key := namespaceOwner{namespace: config.Namespace, scope: config.Scope, userID: config.UserID, agentID: config.AgentID}
		if prior, exists := owners[key]; exists && prior != config.PluginID {
			return fmt.Errorf("%w: namespace %q is claimed by %s and %s for %s/%s/%s", ErrLegacyMigrationConflict, config.Namespace, prior, config.PluginID, config.Scope, config.UserID, config.AgentID)
		}
		owners[key] = config.PluginID
	}
	return nil
}

func findConfigByID(configs []Config, id string) (Config, bool) {
	index := slices.IndexFunc(configs, func(config Config) bool { return config.ID == id })
	if index < 0 {
		return Config{}, false
	}
	return configs[index], true
}

type ConfigAccumulator struct {
	config    Config
	hasConfig bool
}

func (a *ConfigAccumulator) addLegacyPlugin(def Definition, row LegacyPlugin, channels []LegacyChannel) error {
	if isBuiltinGoChannel(def) {
		return a.addLegacyChannelCapability(def, row, channels)
	}
	if !a.hasConfig {
		a.config = Config{PluginID: def.ID, Namespace: def.Namespace, Scope: ScopeSystem, Revision: 1}
		a.hasConfig = true
	}
	if a.config.Enabled != nil && *a.config.Enabled != row.Enabled {
		// Two old writers may have disagreed. A stored false is always retained;
		// a true/false conflict resolves to the conservative disabled decision.
		falseValue := false
		a.config.Enabled = &falseValue
	} else {
		value := row.Enabled
		a.config.Enabled = &value
	}
	if err := mergeConfigPayload(&a.config.Payload, row.Config); err != nil {
		return fmt.Errorf("%w: plugin %s config: %w", ErrLegacyMigrationConflict, def.ID, err)
	}
	return nil
}

func isBuiltinGoChannel(def Definition) bool {
	if def.Source != SourceBuiltin || def.Backend != BackendGo {
		return false
	}
	channelType, ok := strings.CutPrefix(def.ID, "channel/")
	return ok && channelType != "" && !strings.Contains(channelType, "/")
}

func (a *ConfigAccumulator) addLegacyChannelCapability(def Definition, row LegacyPlugin, channels []LegacyChannel) error {
	channelType := strings.TrimPrefix(def.ID, "channel/")
	if hasLegacyPayload(row.Config) && !slices.ContainsFunc(channels, func(channel LegacyChannel) bool {
		return channel.Type == channelType
	}) {
		return fmt.Errorf("%w: channel %s has payload but no %s instance", ErrLegacyMigrationConflict, legacyPluginID(row), channelType)
	}
	if !a.hasConfig {
		a.config = Config{PluginID: def.ID, Namespace: def.Namespace, Scope: ScopeSystem, Revision: 1}
		a.hasConfig = true
	}
	if a.config.Enabled != nil && *a.config.Enabled != row.Enabled {
		falseValue := false
		a.config.Enabled = &falseValue
	} else {
		value := row.Enabled
		a.config.Enabled = &value
	}
	// Channel credentials and instance state belong to channel rows. The old
	// global plugin mirror is only an enabled capability toggle.
	a.config.Payload = json.RawMessage(`{}`)
	return nil
}

func hasLegacyPayload(raw json.RawMessage) bool {
	return len(raw) != 0 && !emptyJSONObject(raw)
}

func (a *ConfigAccumulator) addManifestOverride(def Definition, row LegacyManifestOverride, creatingDefinition bool) error {
	if !a.hasConfig {
		a.config = Config{PluginID: def.ID, Namespace: def.Namespace, Scope: ScopeSystem, Revision: 1}
		a.hasConfig = true
	}
	if row.Enabled != nil {
		if a.config.Enabled != nil && *a.config.Enabled != *row.Enabled {
			falseValue := false
			a.config.Enabled = &falseValue
		} else {
			a.config.Enabled = cloneBool(row.Enabled)
		}
	}
	if row.Config != "" {
		if !creatingDefinition {
			if err := rejectManifestIdentityOverride(def, row.Config); err != nil {
				return err
			}
		}
		payload, err := normalizeManifestConfig(row.Config)
		if err != nil {
			return fmt.Errorf("manifest %s: %w", def.ID, err)
		}
		if err := mergeConfigPayload(&a.config.Payload, payload); err != nil {
			return fmt.Errorf("%w: manifest %s config: %w", ErrLegacyMigrationConflict, def.ID, err)
		}
	}
	if row.SessionEnvVaultKey != "" {
		refs := map[string]any{"name": row.SessionEnvVaultKey, "scope": "system"}
		encoded, err := json.Marshal(refs)
		if err != nil {
			return err
		}
		a.config.CredentialRefs = json.RawMessage(`{"session_env":` + string(encoded) + `}`)
	}
	return nil
}

func mergeConfigPayload(dst *json.RawMessage, raw json.RawMessage) error {
	if len(raw) == 0 || emptyJSONObject(raw) {
		return nil
	}
	var incoming map[string]json.RawMessage
	if err := json.Unmarshal(raw, &incoming); err != nil || incoming == nil {
		return fmt.Errorf("payload must be an object: %w", err)
	}
	var current map[string]json.RawMessage
	if len(*dst) != 0 {
		if err := json.Unmarshal(*dst, &current); err != nil || current == nil {
			return fmt.Errorf("existing payload must be an object: %w", err)
		}
	} else {
		current = make(map[string]json.RawMessage)
	}
	for key, value := range incoming {
		if old, ok := current[key]; ok && !bytes.Equal(bytes.TrimSpace(old), bytes.TrimSpace(value)) {
			return fmt.Errorf("field %q has conflicting values", key)
		}
		current[key] = bytes.Clone(value)
	}
	encoded, err := json.Marshal(current)
	if err != nil {
		return err
	}
	*dst = encoded
	return nil
}

func normalizeManifestConfig(raw string) (json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &object); err != nil || object == nil {
		return nil, fmt.Errorf("invalid JSON object: %w", err)
	}
	// A sparse marker is a storage detail, not backend config. A legacy full
	// snapshot owns every editable field, including omitted fields as null.
	if marker, ok := object["$sparse"]; ok && string(marker) == "true" {
		delete(object, "$sparse")
	} else {
		for _, field := range []string{"name", "display_name", "description", "category", "prompt", "binaries", "skills", "session_env", "oauth_provider"} {
			if _, ok := object[field]; !ok {
				object[field] = json.RawMessage("null")
			}
		}
	}
	for _, field := range []string{"id", "kind", "enabled", "essential", "builtin", "overridden_fields", "name", "display_name"} {
		delete(object, field)
	}
	// Session environment values are literal material in the old snapshot. The
	// vault-key column is a safe locator, but a literal cannot be imported into
	// the target without proving its secret boundary, so fail and preserve the
	// source row for an explicit operator migration.
	if value, ok := object["session_env"]; ok {
		var entries []map[string]json.RawMessage
		if err := json.Unmarshal(value, &entries); err != nil {
			return nil, fmt.Errorf("%w: session_env is not a typed list: %w", ErrLegacyMigrationConflict, err)
		}
		for _, entry := range entries {
			if literal, ok := entry["value"]; ok && !bytes.Equal(bytes.TrimSpace(literal), []byte("null")) {
				return nil, fmt.Errorf("%w: session_env literal value requires explicit vault migration", ErrLegacyMigrationConflict)
			}
		}
	}
	return json.Marshal(object)
}

func rejectManifestIdentityOverride(def Definition, raw string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &object); err != nil || object == nil {
		return fmt.Errorf("%w: manifest %s identity JSON: %w", ErrLegacyMigrationConflict, def.ID, err)
	}
	for field, expected := range map[string]string{"name": def.Namespace, "display_name": def.DisplayName} {
		value, ok := object[field]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			continue
		}
		actual := jsonString(value)
		if actual != expected {
			return fmt.Errorf("%w: manifest %s owns %s=%q; identity changes need explicit mapping", ErrLegacyMigrationConflict, def.ID, field, actual)
		}
	}
	return nil
}

func customDefinitionFromManifest(id, raw string) (Definition, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &object); err != nil || object == nil {
		return Definition{}, fmt.Errorf("%w: manifest %s JSON: %w", ErrLegacyMigrationConflict, id, err)
	}
	name := jsonString(object["name"])
	if name == "" {
		name = id
	}
	defSpec, err := normalizeManifestConfig(raw)
	if err != nil {
		return Definition{}, fmt.Errorf("manifest %s: %w", id, err)
	}
	displayName := jsonString(object["display_name"])
	if displayName == "" {
		displayName = name
	}
	kind := jsonString(object["kind"])
	if kind == "go" {
		return Definition{}, fmt.Errorf("%w: custom Go implementation %s", ErrLegacyMigrationConflict, id)
	}
	return Definition{
		ID: id, Namespace: sanitizeLegacyIdent(name, "plugin"), DisplayName: displayName,
		Backend: BackendCLI, Source: SourceCustom, ImplementationKey: "cli",
		Spec: defSpec, DefaultEnabled: false, Revision: 1,
	}, nil
}

func jsonString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func normalizeMCP(row LegacyMCPRegistration) (Definition, Config, error) {
	parsedID, err := uuid.Parse(row.ID)
	if err != nil {
		return Definition{}, Config{}, fmt.Errorf("%w: MCP id %q: %w", ErrLegacyMigrationConflict, row.ID, err)
	}
	if !validLegacyScope(row.Scope) || !legacyOwnerMatches(row.Scope, row.UserID, row.AgentID) {
		return Definition{}, Config{}, fmt.Errorf("%w: MCP %s has invalid owner tuple", ErrLegacyMigrationConflict, row.ID)
	}
	if row.Name == "" || row.URL == "" || (row.Transport != legacyMCPTransportSSE && row.Transport != legacyMCPTransportHTTP) {
		return Definition{}, Config{}, fmt.Errorf("%w: MCP %s has invalid name, URL, or transport", ErrLegacyMigrationConflict, row.ID)
	}
	if !validLegacyMCPAuth(row.AuthType) || (row.CredentialMode != "" && !validLegacyMCPCredentialMode(row.CredentialMode)) {
		return Definition{}, Config{}, fmt.Errorf("%w: MCP %s has invalid auth or credential mode", ErrLegacyMigrationConflict, row.ID)
	}
	if err := validateLegacyMCPCredentialRefs(row, parsedID); err != nil {
		return Definition{}, Config{}, err
	}
	namespace := sanitizeLegacyIdent(row.Name, "mcp")
	if _, err := normalizeMCPTools(namespace, row.Tools); err != nil {
		return Definition{}, Config{}, fmt.Errorf("MCP %s tools: %w", row.ID, err)
	}
	metadata, err := normalizeMCPMetadata(row.Metadata)
	if err != nil {
		return Definition{}, Config{}, fmt.Errorf("MCP %s metadata: %w", row.ID, err)
	}
	if row.AuthType == legacyMCPAuthOAuth && row.OAuthClientSecretExists && metadataString(row.Metadata, "oauth.client_id") == "" {
		return Definition{}, Config{}, fmt.Errorf("%w: MCP %s OAuth client secret exists without a public client_id", ErrLegacyMigrationConflict, row.ID)
	}
	payload := map[string]any{
		"url": row.URL, "transport": row.Transport, "auth_type": row.AuthType,
		"credential_mode": effectiveCredentialMode(row.CredentialMode), "metadata": metadata,
	}
	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		return Definition{}, Config{}, err
	}
	refs := mcpCredentialRefs(row, parsedID)
	creator := ""
	if row.Scope == string(ScopeUser) || row.Scope == string(ScopeUserAgent) {
		creator = row.UserID
	}
	def := Definition{
		ID: "custom/" + row.ID, Namespace: namespace, DisplayName: row.Name,
		Backend: BackendMCP, Source: SourceCustom, ImplementationKey: "mcp", Spec: json.RawMessage(`{}`),
		DefaultEnabled: false, Revision: 1, CreatorUserID: creator,
	}
	config := Config{
		ID: row.ID, PluginID: def.ID, Namespace: namespace, Scope: Scope(row.Scope), UserID: row.UserID, AgentID: row.AgentID,
		Enabled: cloneBool(&row.Enabled), Payload: encodedPayload, CredentialRefs: refs, Revision: 1,
	}
	return def, config, nil
}

func validateLegacyMCPCredentialRefs(row LegacyMCPRegistration, id uuid.UUID) error {
	derivedBearer := legacyBearerCredentialName(id)
	switch row.AuthType {
	case legacyMCPAuthBearer:
		if row.CredentialRef != derivedBearer {
			return fmt.Errorf("%w: MCP %s bearer credential must use derived locator", ErrLegacyMigrationConflict, id)
		}
	case legacyMCPAuthNone, legacyMCPAuthOAuth:
		if row.CredentialRef != "" {
			return fmt.Errorf("%w: MCP %s has a credential locator for auth type %q", ErrLegacyMigrationConflict, id, row.AuthType)
		}
	}
	if row.AuthType == legacyMCPAuthOAuth {
		if ref := metadataString(row.Metadata, "oauth.client_secret_ref", "client_secret_ref"); ref != "" && ref != legacyOAuthClientSecretName(row.ID) {
			return fmt.Errorf("%w: MCP %s OAuth client secret locator is not derived from registration identity", ErrLegacyMigrationConflict, id)
		}
	}
	return nil
}

func normalizeMCPTools(namespace string, raw json.RawMessage) ([]map[string]string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var tools []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, err
	}
	seen := make(map[string]string, len(tools))
	result := make([]map[string]string, 0, len(tools))
	for _, tool := range tools {
		local := sanitizeLegacyIdent(tool.Name, "tool")
		if prior, ok := seen[local]; ok {
			return nil, fmt.Errorf("local tool names %q and %q collide as %q", prior, tool.Name, local)
		}
		seen[local] = tool.Name
		name, err := ExportedToolName(namespace, local)
		if err != nil {
			return nil, err
		}
		result = append(result, map[string]string{"remote_name": tool.Name, "local_name": local, "exported_name": name})
	}
	return result, nil
}

func mcpCredentialRefs(row LegacyMCPRegistration, id uuid.UUID) json.RawMessage {
	refs := make(map[string]any)
	if row.CredentialRef != "" {
		refs["bearer"] = map[string]any{"name": legacyBearerCredentialName(id), "scope": row.Scope, "user_id": row.UserID, "agent_id": row.AgentID}
	}
	if row.AuthType == legacyMCPAuthOAuth {
		bundle := legacyOAuthBundleName(id)
		bundleRef := map[string]any{"name": bundle, "mode": effectiveCredentialMode(row.CredentialMode)}
		if effectiveCredentialMode(row.CredentialMode) == legacyMCPCredPerUser {
			// A registration row does not identify every per-user grant. Never
			// claim that its registration tuple owns those user grants.
			bundleRef["owner"] = "per_user"
		} else {
			bundleRef["scope"], bundleRef["user_id"], bundleRef["agent_id"] = row.Scope, row.UserID, row.AgentID
		}
		refs["oauth_bundle"] = bundleRef
		if row.OAuthClientSecretExists {
			refs["oauth_client_secret"] = map[string]any{"name": legacyOAuthClientSecretName(row.ID), "scope": row.Scope, "user_id": row.UserID, "agent_id": row.AgentID}
		}
	}
	encoded, _ := json.Marshal(refs)
	return encoded
}

func legacyBearerCredentialName(id uuid.UUID) string {
	return "MCP_TOKEN_" + strings.ToUpper(strings.ReplaceAll(id.String(), "-", "_"))
}

func legacyOAuthBundleName(id uuid.UUID) string {
	return "MCP_OAUTH_" + strings.ToUpper(strings.ReplaceAll(id.String(), "-", "_"))
}

func legacyOAuthClientSecretName(id string) string {
	return "MCP_OAUTH_CLIENT_" + strings.ToUpper(strings.ReplaceAll(id, "-", "_"))
}

func metadataString(metadata map[string]any, keys ...string) string {
	for _, key := range keys {
		parts := strings.Split(key, ".")
		var current any = metadata
		for _, part := range parts {
			object, ok := current.(map[string]any)
			if !ok {
				current = nil
				break
			}
			current = object[part]
		}
		if value, ok := current.(string); ok {
			return value
		}
	}
	return ""
}

func normalizeMCPMetadata(metadata map[string]any) (map[string]any, error) {
	if metadata == nil {
		return map[string]any{}, nil
	}
	result := make(map[string]any, len(metadata))
	for key, value := range metadata {
		switch key {
		case "call_timeout_seconds":
			if _, ok := value.(float64); !ok {
				return nil, fmt.Errorf("%w: metadata.%s must be a number", ErrLegacyMigrationConflict, key)
			}
			result[key] = value
		case "oauth":
			nested, err := normalizeMCPMetadataOAuth(value)
			if err != nil {
				return nil, err
			}
			result[key] = nested
		case "registry":
			nested, err := normalizeMCPMetadataRegistry(value)
			if err != nil {
				return nil, err
			}
			result[key] = nested
		default:
			return nil, fmt.Errorf("%w: unsupported MCP metadata field %q", ErrLegacyMigrationConflict, key)
		}
	}
	return result, nil
}

func normalizeMCPMetadataOAuth(value any) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: metadata.oauth must be an object", ErrLegacyMigrationConflict)
	}
	result := make(map[string]any, len(object))
	for key, entry := range object {
		if key != "client_id" && key != "client_secret_ref" {
			return nil, fmt.Errorf("%w: unsupported MCP metadata.oauth field %q", ErrLegacyMigrationConflict, key)
		}
		if _, ok := entry.(string); !ok {
			return nil, fmt.Errorf("%w: metadata.oauth.%s must be a string", ErrLegacyMigrationConflict, key)
		}
		if key == "client_id" {
			result[key] = entry
		}
		// client_secret_ref is carried exclusively by CredentialRefs.
	}
	return result, nil
}

func normalizeMCPMetadataRegistry(value any) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: metadata.registry must be an object", ErrLegacyMigrationConflict)
	}
	result := make(map[string]any, len(object))
	for key, entry := range object {
		switch key {
		case "source", "id", "version", "installed_at":
			if _, ok := entry.(string); !ok {
				return nil, fmt.Errorf("%w: metadata.registry.%s must be a string", ErrLegacyMigrationConflict, key)
			}
		default:
			return nil, fmt.Errorf("%w: unsupported MCP metadata.registry field %q", ErrLegacyMigrationConflict, key)
		}
		result[key] = entry
	}
	return result, nil
}

// ConvertLegacyToolOverride resolves the old mcp__server__tool key against the
// exact legacy registration owner. Core overrides are returned as zero-value
// migrations and are written with their durable core tool name by the importer.
func ConvertLegacyToolOverride(override LegacyToolOverride, registrations []LegacyMCPRegistration, metadata *toolmeta.Registry) (ToolOverrideMigration, error) {
	if !validLegacyScope(override.Scope) || !legacyOwnerMatches(override.Scope, override.UserID, override.AgentID) {
		return ToolOverrideMigration{}, ErrLegacyMigrationConflict
	}
	// This finite migration table comes from the old generated catalog. It is
	// not a runtime prefix authorization rule.
	if metadata != nil {
		var match toolmeta.ActionTool
		matched := false
		for _, spec := range metadata.Tools() {
			if spec.PluginID == "" || spec.Namespace == "" || spec.LocalName == "" {
				continue
			}
			legacyName := spec.Family + "_" + spec.LocalName
			if override.ToolName != legacyName {
				continue
			}
			if matched && (match.PluginID != spec.PluginID || match.Namespace != spec.Namespace || match.LocalName != spec.LocalName) {
				return ToolOverrideMigration{}, fmt.Errorf("%w: legacy Go tool %q maps to multiple plugin identities", ErrLegacyMigrationConflict, override.ToolName)
			}
			match, matched = spec, true
		}
		if matched {
			return ToolOverrideMigration{
				LegacyID: override.ID, OldName: override.ToolName,
				NewName: match.Name, PluginID: match.PluginID, Namespace: match.Namespace, LocalTool: match.LocalName,
				Scope: Scope(override.Scope), UserID: override.UserID, AgentID: override.AgentID, Enabled: override.Enabled,
			}, nil
		}
	}

	server, remote, ok := splitLegacyMCPToolName(override.ToolName)
	if !ok {
		if strings.HasPrefix(override.ToolName, "mcp__") {
			return ToolOverrideMigration{}, fmt.Errorf("%w: malformed MCP tool override %q", ErrLegacyMigrationConflict, override.ToolName)
		}
		return ToolOverrideMigration{}, nil
	}
	var matches []LegacyMCPRegistration
	for _, registration := range registrations {
		if !validLegacyScope(registration.Scope) || !legacyOwnerMatches(registration.Scope, registration.UserID, registration.AgentID) {
			return ToolOverrideMigration{}, ErrLegacyMigrationConflict
		}
		// A policy can constrain a broader shared registration. Candidates are
		// scopes that can intersect in an execution context, not identical tuples.
		if override.UserID != "" && registration.UserID != "" && override.UserID != registration.UserID {
			continue
		}
		if override.AgentID != "" && registration.AgentID != "" && override.AgentID != registration.AgentID {
			continue
		}
		if sanitizeLegacyIdent(registration.Name, "mcp") != server {
			continue
		}
		var tools []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(registration.Tools, &tools); err != nil {
			return ToolOverrideMigration{}, fmt.Errorf("%w: MCP %s tools catalog: %w", ErrLegacyMigrationConflict, registration.ID, err)
		}
		for _, tool := range tools {
			if sanitizeLegacyIdent(tool.Name, "tool") == remote {
				matches = append(matches, registration)
			}
		}
	}
	if len(matches) != 1 {
		return ToolOverrideMigration{}, fmt.Errorf("%w: %s matches %d MCP registrations", ErrLegacyMigrationConflict, override.ToolName, len(matches))
	}
	registration := matches[0]
	local := sanitizeLegacyIdent(remote, "tool")
	newName, err := ExportedToolName(sanitizeLegacyIdent(registration.Name, "mcp"), local)
	if err != nil {
		return ToolOverrideMigration{}, err
	}
	return ToolOverrideMigration{
		LegacyID: override.ID, OldName: override.ToolName, NewName: newName,
		PluginID: "custom/" + registration.ID, ConfigID: registration.ID, Namespace: sanitizeLegacyIdent(registration.Name, "mcp"), LocalTool: local,
		Scope: Scope(override.Scope), UserID: override.UserID, AgentID: override.AgentID, Enabled: override.Enabled,
	}, nil
}

func validLegacyScope(scope string) bool {
	switch Scope(scope) {
	case ScopeSystem, ScopeSystemAgent, ScopeUser, ScopeUserAgent:
		return true
	default:
		return false
	}
}

func legacyOwnerMatches(scope, userID, agentID string) bool {
	switch Scope(scope) {
	case ScopeSystem:
		return userID == "" && agentID == ""
	case ScopeSystemAgent:
		return userID == "" && agentID != ""
	case ScopeUser:
		return userID != "" && agentID == ""
	case ScopeUserAgent:
		return userID != "" && agentID != ""
	default:
		return false
	}
}

func effectiveCredentialMode(mode string) string {
	if mode == "" {
		return legacyMCPCredShared
	}
	return mode
}

func sanitizeLegacyIdent(value, fallback string) string {
	var builder strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('_')
		}
	}
	result := strings.Trim(builder.String(), "_")
	if result == "" {
		result = fallback
	}
	return result
}

func validLegacyMCPAuth(auth string) bool {
	return auth == legacyMCPAuthNone || auth == legacyMCPAuthBearer || auth == legacyMCPAuthOAuth
}

func validLegacyMCPCredentialMode(mode string) bool {
	return mode == legacyMCPCredShared || mode == legacyMCPCredPerUser
}

func splitLegacyMCPToolName(name string) (server, tool string, ok bool) {
	const prefix = "mcp__"
	if !strings.HasPrefix(name, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(name, prefix)
	separator := strings.Index(rest, "__")
	if separator <= 0 || separator+2 >= len(rest) {
		return "", "", false
	}
	return rest[:separator], rest[separator+2:], true
}
