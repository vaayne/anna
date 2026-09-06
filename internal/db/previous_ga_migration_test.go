package db

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/CherryHQ/stella/internal/agent/session"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
)

const (
	// Keep these boundaries explicit. Advancing either one without adding the
	// representative fixture/assertions for the newly crossed migrations turns
	// this test into a green lie.
	previousGAVersion = int64(20260725161331)
	// Library V1, channel guest sessions/indexes, channel allowlist backfill,
	// session activity, per-message actor provenance and summary authority,
	// the durable Session inbox, restrictive Library ownership, and the Discord
	// explicit guild-access backfill, optimistic group-dispatch plumbing, the
	// reply-to-wake optimistic cutover, per-call LLM usage accounting, the group
	// context event/trigger origin columns, the group-history BM25 index, the
	// retired group-memory table, the generalized media owner, the media
	// baseline column, the Settings tool prefix cutover, the model catalog table,
	// and per-call reasoning token accounting are the post-anchor migrations
	// exercised below. Library chunk locator integrity, the dedicated Skill Home
	// cutover evidence schema, retired RTK plugin cleanup, retired Settings tool
	// override cleanup, the built-in Stella Settings default, retired
	// webfetch override cleanup, retired tap-web plugin cleanup, and the dropped
	// plugin scheduler columns are checked explicitly.
	currentMigrationVersion = sequentialAnchor + 40
	latestMigrationVersion  = sequentialAnchor + 42

	previousGAUserID                     = "00000000-0000-0000-0000-000000000001"
	previousGAGroupID                    = "00000000-0000-0000-0000-000000000002"
	previousGAOlderChatID                = "00000000-0000-0000-0000-000000000009"
	previousGAOldChatID                  = "00000000-0000-0000-0000-000000000003"
	previousGANewChatID                  = "00000000-0000-0000-0000-000000000004"
	previousGAMessageID                  = "00000000-0000-0000-0000-000000000005"
	previousGAPartID                     = "00000000-0000-0000-0000-000000000006"
	previousGAMediaID                    = "00000000-0000-0000-0000-000000000007"
	previousGAWebhookID                  = "00000000-0000-0000-0000-000000000008"
	previousGADelegateChatID             = "00000000-0000-0000-0000-000000000051"
	previousGASchedulerChatID            = "00000000-0000-0000-0000-000000000052"
	previousGATaskChatID                 = "00000000-0000-0000-0000-000000000053"
	previousGADelegateMsgID              = "00000000-0000-0000-0000-000000000054"
	previousGASchedulerMsgID             = "00000000-0000-0000-0000-000000000055"
	previousGATaskMsgID                  = "00000000-0000-0000-0000-000000000056"
	previousGALibraryFile                = "00000000-0000-0000-0000-000000000041"
	previousGAAgentLibraryFile           = "00000000-0000-0000-0000-000000000047"
	previousGAChunkSet                   = "00000000-0000-0000-0000-000000000042"
	previousGAChunk                      = "00000000-0000-0000-0000-000000000043"
	previousGAGuestID                    = "00000000-0000-0000-0000-000000000044"
	previousGAGuestChatID                = "00000000-0000-0000-0000-000000000045"
	previousGAAllowGroupDiscordChannelID = "previous-ga-discord-allow-group"
	previousGAAgentID                    = "previous-ga-agent"
	previousGAPluginJobID                = "previous-ga-plugin-job"
	previousGAUserJobID                  = "previous-ga-user-job"
	previousGACascadeAgentID             = "previous-ga-cascade-agent"
	previousGALibraryAgentID             = "previous-ga-library-agent"
	previousGAProviderID                 = "previous-ga-provider"
	previousGASkillID                    = "previous-ga-skill"
	previousGAOlderSession               = "previous-ga-agent:group:00000000-0000-0000-0000-000000000002:zz"
	previousGAOldSession                 = "previous-ga-agent:group:00000000-0000-0000-0000-000000000002:a"
	previousGANewSession                 = "previous-ga-agent:group:00000000-0000-0000-0000-000000000002:z"
)

var previousGATime = time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)

// TestPreviousGAPostgresForwardMigration builds the exact v0.60.4 goose
// boundary from immutable migration history, then uses the production OpenDB
// path to upgrade persisted rows through every candidate migration.
func TestPreviousGAPostgresForwardMigration(t *testing.T) {
	ctx := context.Background()
	candidate := PreviousGAUpgradedDBForTest(t)
	assertPreviousGAUpgrade(t, ctx, candidate)
}

// PreviousGAUpgradedDBForTest exposes the real forward-migration fixture to
// external-package integration tests that cannot import LCM from package db
// without creating the lcm -> eventlog -> db import cycle.
func PreviousGAUpgradedDBForTest(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	dsn, legacy := newPreviousGADB(t, ctx)
	seedPreviousGAData(t, ctx, legacy)
	legacy.Close()

	candidate, err := OpenDB(dsn, WithMaxConns(4))
	if err != nil {
		t.Fatalf("OpenDB upgrades v0.60.4 database: %v", err)
	}
	t.Cleanup(candidate.Close)
	return candidate
}

// newPreviousGADB intentionally starts with an empty database instead of
// rolling a current template back: Down migrations are not a historical schema
// reconstruction contract, while UpTo is the immutable v0.60.4 boundary.
func newPreviousGADB(t *testing.T, ctx context.Context) (string, *pgxpool.Pool) {
	t.Helper()
	pkgTestEnsure()
	if pkgTestErr != nil {
		t.Fatalf("start embedded PostgreSQL: %v", pkgTestErr)
	}

	name := fmt.Sprintf("previous_ga_%d", pkgTestSeq.Add(1))
	if _, err := pkgTestAdmin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create previous-GA database: %v", err)
	}
	dsn := pkgTestServer.DSNFor(name)
	legacy, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open previous-GA database: %v", err)
	}
	t.Cleanup(func() {
		legacy.Close()
		_, _ = pkgTestAdmin.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", name)
		_, _ = pkgTestAdmin.Exec(ctx, "DROP DATABASE IF EXISTS "+name)
	})

	conn, err := legacy.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire previous-GA migration connection: %v", err)
	}
	err = ensureExtensions(ctx, conn)
	conn.Release()
	if err != nil {
		t.Fatalf("install previous-GA extensions: %v", err)
	}

	migrations, err := fs.Sub(MigrationsFS, "migrations")
	if err != nil {
		t.Fatalf("open migrations: %v", err)
	}
	sqlDB := stdlib.OpenDBFromPool(legacy)
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations)
	if err != nil {
		t.Fatalf("create migration provider: %v", err)
	}
	if _, err := provider.UpTo(ctx, previousGAVersion); err != nil {
		t.Fatalf("migrate to v0.60.4 boundary: %v", err)
	}
	var appliedVersion int64
	if err := legacy.QueryRow(ctx, `SELECT version_id FROM goose_db_version WHERE is_applied ORDER BY id DESC LIMIT 1`).Scan(&appliedVersion); err != nil {
		t.Fatalf("read previous-GA migration ledger: %v", err)
	}
	if appliedVersion != previousGAVersion {
		t.Fatalf("previous-GA migration boundary = %d, want %d", appliedVersion, previousGAVersion)
	}
	return dsn, legacy
}

func seedPreviousGAData(t *testing.T, ctx context.Context, db *pgxpool.Pool) {
	t.Helper()
	exec := func(name, sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	exec("user", `INSERT INTO auth_user (id, email, created_at, updated_at) VALUES ($1, 'previous-ga@test.invalid', $2, $2)`, previousGAUserID, previousGATime)
	exec("personal access token", `INSERT INTO personal_access_token (id, public_id, user_id, name, token_hash, last4, scopes, created_at, updated_at)
		VALUES ('00000000-0000-0000-0000-000000000014', 'previous-ga-pat', $1, 'previous GA PAT', 'previous-ga-hash', '1234', ARRAY['goals:read'], $2, $2)`, previousGAUserID, previousGATime)
	exec("agents", `INSERT INTO agent (id, name, workspace, enabled_builtin_skills, created_at, updated_at) VALUES
		($1, 'Previous GA Agent', '/tmp', '["historical-allowlist-entry"]'::jsonb, $4, $4),
		($2, 'Previous GA Cascade Agent', '/tmp', 'null'::jsonb, $4, $4),
		($3, 'Stella', '/tmp/stella', 'null'::jsonb, $4, $4)`, previousGAAgentID, previousGACascadeAgentID, "stella", previousGATime)
	exec("canonical provider", `INSERT INTO provider (id, type, name, config, created_at, updated_at) VALUES ($1, 'anthropic', 'Previous GA Provider', $2, $3, $3)`, previousGAProviderID, `{"api_key":"previous-ga-key"}`, previousGATime)

	// The pre-unification model settings: a standalone vision row, and an
	// embedding block whose bare model id is paired with one provider's key.
	exec("legacy vision setting", `INSERT INTO app_setting (key, value, created_at, updated_at) VALUES ('vision', $1, $2, $2)`, `{"model":"previous-ga-provider/claude-vision"}`, previousGATime)
	exec("legacy embedding setting", `INSERT INTO app_setting (key, value, created_at, updated_at) VALUES ('embedding', $1, $2, $2)`, `{"enabled":true,"model":"text-embedding-3-small","dim":1536,"api_key":"previous-ga-key","normalize":true}`, previousGATime)
	exec("legacy plugin rows", `INSERT INTO plugin (id, kind, name, created_at, updated_at) VALUES
		('sandbox/local', 'sandbox', 'Local sandbox', $1, $1),
		('sandbox', 'sandbox', 'Sandbox near miss', $1, $1),
		('hook/rtk', 'hook', 'RTK', $1, $1),
		('tool/tap-web', 'tool', 'Tap Web', $1, $1),
		('tool/unrelated', 'tool', 'Unrelated tool', $1, $1)`, previousGATime)
	exec("legacy plugin state rows", `INSERT INTO plugin_state (plugin_id, scope_kind, state_key, value, created_at, updated_at) VALUES
		('sandbox/local', 'system', 'sandbox-state', '{"keep":"no"}', $1, $1),
		('sandbox', 'system', 'near-miss-state', '{"keep":"yes"}', $1, $1),
		('hook/rtk', 'system', 'rtk-state', '{"keep":"no"}', $1, $1),
		('tool/tap-web', 'system', 'tap-web-state', '{"keep":"no"}', $1, $1),
		('tool/unrelated', 'system', 'unrelated-state', '{"keep":"yes"}', $1, $1)`, previousGATime)
	// A plugin-owned scheduler row from the removed plugin scheduler capability,
	// alongside a user subscription row that must survive with its job_key.
	exec("legacy scheduler jobs", `INSERT INTO sched_job (id, owner_kind, exec_scope, plugin_id, job_key, runtime_name, name, message, created_at, updated_at) VALUES
		($1, 'plugin', 'system', 'tool/legacy', 'refresh-cache', 'bot', 'Refresh Cache', '', $3, $3),
		($2, 'user', 'user', '', 'digest', '', 'Digest', 'run the digest', $3, $3)`,
		previousGAPluginJobID, previousGAUserJobID, previousGATime)

	exec("legacy mutable Skill", `
		INSERT INTO skill (id, scope, user_id, agent_id, name, description, status, disable_model_invocation, metadata, created_at, updated_at)
		VALUES ($1, 'user_agent', $2, $3, 'Previous GA / Skill', 'legacy description', 'active', true, '{"created_by":"reflect"}', $4, $4)`,
		previousGASkillID, previousGAUserID, previousGAAgentID, previousGATime)
	exec("legacy mutable Skill files", `
		INSERT INTO skill_file (skill_id, path, content) VALUES
			($1, 'SKILL.md', '# Previous GA Skill'),
			($1, 'references/raw.bin', E'\\x00ff78')`, previousGASkillID)
	exec("group", `INSERT INTO ctx_group_state (id, platform, platform_group_id, created_at, updated_at) VALUES ($1, 'test', 'previous-ga-group', $2, $2)`, previousGAGroupID, previousGATime)
	exec("duplicate group chats", `
		INSERT INTO ctx_conversation (id, session_id, channel, kind, archived, last_active, agent_id, user_id, group_id, created_at, updated_at)
		VALUES
			($1, $2, 'web', 'chat', false, $3, $4, $5::text, $6, $7, $7),
			($8, $9, 'web', 'chat', false, $10, $4, $5::text, $6, $7, $7),
			($11, $12, 'web', 'chat', false, $10, $4, $5::text, $6, $7, $7)`,
		previousGAOlderChatID, previousGAOlderSession, previousGATime.Add(-time.Hour), previousGAAgentID, previousGAGroupID, previousGAGroupID, previousGATime,
		previousGAOldChatID, previousGAOldSession, previousGATime,
		previousGANewChatID, previousGANewSession)
	exec("legacy message", `INSERT INTO ctx_message (id, conversation_id, seq, role, content, token_count, created_at) VALUES ($1, $2, 1, 'user', 'legacy media parent', 1, $3)`, previousGAMessageID, previousGANewChatID, previousGATime)
	exec("legacy message part", `INSERT INTO ctx_message_part (id, message_id, part_type, ordinal, text_content) VALUES ($1, $2, 'text', 0, 'legacy media child')`, previousGAPartID, previousGAMessageID)
	exec("legacy internal conversations", `
		INSERT INTO ctx_conversation (id, session_id, channel, kind, archived, last_active, agent_id, user_id, created_at, updated_at)
		VALUES
			($1, 'previous-ga-delegate', 'delegate', $4, false, $7, $8, $9, $7, $7),
			($2, 'previous-ga-scheduler', 'scheduler', $5, false, $7, $8, $9, $7, $7),
			($3, 'previous-ga-task', 'task', $6, false, $7, $8, $9, $7, $7)`,
		previousGADelegateChatID, previousGASchedulerChatID, previousGATaskChatID,
		string(session.KindDelegate), string(session.KindScheduler), string(session.KindTask),
		previousGATime, previousGAAgentID, previousGAUserID)
	exec("legacy internal user-role messages", `
		INSERT INTO ctx_message (id, conversation_id, seq, role, content, token_count, created_at)
		VALUES
			($1, $4, 1, 'user', 'legacy delegate input', 1, $7),
			($2, $5, 1, 'user', 'legacy scheduler input', 1, $7),
			($3, $6, 1, 'user', 'legacy task input', 1, $7)`,
		previousGADelegateMsgID, previousGASchedulerMsgID, previousGATaskMsgID,
		previousGADelegateChatID, previousGASchedulerChatID, previousGATaskChatID, previousGATime)
	exec("legacy internal context items", `
		INSERT INTO ctx_item (conversation_id, ordinal, item_type, message_id, event_type, role, created_at)
		VALUES
			($1, 1, 'message', $4, 'text', 'user', $7),
			($2, 1, 'message', $5, 'text', 'user', $7),
			($3, 1, 'message', $6, 'text', 'user', $7)`,
		previousGADelegateChatID, previousGASchedulerChatID, previousGATaskChatID,
		previousGADelegateMsgID, previousGASchedulerMsgID, previousGATaskMsgID, previousGATime)

	exec("vault entries", `
		INSERT INTO vault_entry (id, scope, name, ciphertext, created_at, updated_at) VALUES
			('00000000-0000-0000-0000-000000000011', 'system', 'LARK_CLI_OAUTH', 'lark-cipher', $1, $1),
			('00000000-0000-0000-0000-000000000012', 'system', 'FEISHU_CLI_OAUTH', 'feishu-cipher', $1, $1),
			('00000000-0000-0000-0000-000000000013', 'system', 'CUSTOM_SECRET', 'custom-cipher', $1, $1)`, previousGATime)
	exec("OAuth providers", `
		INSERT INTO plugin_oauth_provider (id, provider_id, client_id, scopes, created_at, updated_at) VALUES
			('00000000-0000-0000-0000-000000000021', 'lark', 'lark-client', ARRAY['calendar'], $1, $1),
			('00000000-0000-0000-0000-000000000022', 'feishu', 'feishu-client', ARRAY['drive'], $1, $1),
			('00000000-0000-0000-0000-000000000023', 'custom', 'custom-client', ARRAY['custom'], $1, $1)`, previousGATime)
	exec("Lark override", `
		INSERT INTO plugin_override (plugin_id, enabled, session_env_vault_key, config, created_at, updated_at)
		VALUES ('tool/lark-cli', true, 'custom-vault-key',
			'{"prompt":"custom prompt", "oauth_provider":"feishu", "session_env":{"TOKEN":"secret"}, "binary":"lark-cli", "custom":"keep"}', $1, $1)`, previousGATime)
	exec("unrelated and retired overrides", `
		INSERT INTO plugin_override (plugin_id, enabled, config, created_at, updated_at) VALUES
			('tool/custom', false, '{"custom":"untouched"}', $1, $1),
			('hook/rtk', true, '{"name":"rtk"}', $1, $1),
			('tool/tap-web', true, '{"name":"tap-web"}', $1, $1)`, previousGATime)
	exec("legacy Discord channel relying on allow_group alone", `
		INSERT INTO channel (id, name, type, enabled, config, created_at, updated_at)
		VALUES ($1, 'Previous GA Discord Allow Group', 'discord', true, '{"allow_group": true, "token": "legacy-token"}', $2, $2)`,
		previousGAAllowGroupDiscordChannelID, previousGATime)
}

func assertPreviousGAUpgrade(t *testing.T, ctx context.Context, db *pgxpool.Pool) {
	t.Helper()
	count := func(name, query string, args ...any) int {
		t.Helper()
		var got int
		if err := db.QueryRow(ctx, query, args...).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		return got
	}
	var stellaSettingsEnabled bool
	if err := db.QueryRow(ctx, `SELECT system_settings_tools_enabled FROM agent WHERE id = 'stella'`).Scan(&stellaSettingsEnabled); err != nil {
		t.Fatalf("read migrated built-in Stella Settings policy: %v", err)
	}
	if !stellaSettingsEnabled {
		t.Fatal("built-in Stella Settings tools remain disabled after migration")
	}

	for _, agentID := range []string{previousGAAgentID, previousGACascadeAgentID} {
		var policy string
		if err := db.QueryRow(ctx, `SELECT enabled_builtin_skills::text FROM agent WHERE id = $1`, agentID).Scan(&policy); err != nil {
			t.Fatalf("read migrated Agent Skill policy for %s: %v", agentID, err)
		}
		if policy != `{"version": 1, "disabled": []}` {
			t.Fatalf("migrated Agent Skill policy for %s = %s, want canonical empty v1", agentID, policy)
		}
	}
	for _, invalid := range []string{
		`null`,
		`[]`,
		`{"version":1.0,"disabled":[]}`,
		`{"version":1,"disabled":[1]}`,
		`{"version":1,"disabled":[null]}`,
		`{"version":1,"disabled":[{"system":"a"}]}`,
		`{"version":1,"disabled":["system:z","builtin:a"]}`,
		`{"version":1,"disabled":["system:a","system:a"]}`,
		`{"version":1,"disabled":["user:a"]}`,
		`{"version":1,"disabled":["system:Upper"]}`,
		`{"version":1,"disabled":["system:` + strings.Repeat("a", 65) + `"]}`,
	} {
		if _, err := db.Exec(ctx, `UPDATE agent SET enabled_builtin_skills = $1::jsonb WHERE id = $2`, invalid, previousGAAgentID); err == nil {
			t.Errorf("canonical Agent Skill policy constraint accepted %s", invalid)
		}
	}
	const validPolicy = `{"version":1,"disabled":["builtin:a","system:b","system_agent:c"]}`
	if _, err := db.Exec(ctx, `UPDATE agent SET enabled_builtin_skills = $1::jsonb WHERE id = $2`, validPolicy, previousGACascadeAgentID); err != nil {
		t.Fatalf("canonical Agent Skill policy constraint rejected valid policy: %v", err)
	}
	var tokenUse string
	var issuedByProvisioning bool
	if err := db.QueryRow(ctx, `SELECT token_use, issued_by_provisioning FROM personal_access_token WHERE public_id = 'previous-ga-pat'`).Scan(&tokenUse, &issuedByProvisioning); err != nil {
		t.Fatalf("read migrated personal access token use: %v", err)
	}
	if tokenUse != "personal" || issuedByProvisioning {
		t.Fatalf("migrated personal access token use=%q issued_by_provisioning=%v, want personal/false", tokenUse, issuedByProvisioning)
	}
	var legacySkillName string
	var legacySkillFileCount, legacySkillBytes int64
	if err := db.QueryRow(ctx, `
		SELECT s.name, count(f.path), sum(octet_length(f.content))
		FROM skill s JOIN skill_file f ON f.skill_id=s.id
		WHERE s.id=$1 GROUP BY s.name`, previousGASkillID).Scan(&legacySkillName, &legacySkillFileCount, &legacySkillBytes); err != nil {
		t.Fatalf("read previous-GA mutable Skill after schema migration: %v", err)
	}
	if legacySkillName != "Previous GA / Skill" || legacySkillFileCount != 2 || legacySkillBytes <= 0 {
		t.Fatalf("previous-GA Skill = name %q files %d bytes %d", legacySkillName, legacySkillFileCount, legacySkillBytes)
	}
	if got := count("Skill Home migration evidence table", `SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='skill_home_migration'`); got != 1 {
		t.Fatalf("Skill Home migration evidence tables = %d, want 1", got)
	}
	if got := count("uninitialized Skill Home migration evidence", `SELECT count(*) FROM skill_home_migration`); got != 0 {
		t.Fatalf("ordinary schema migration wrote %d Skill cutover markers, want 0", got)
	}
	var allowAllGuilds bool
	var hasAllowedGuildIDsKey bool
	if err := db.QueryRow(ctx, `
		SELECT (config::jsonb ->> 'allow_all_guilds')::boolean, config::jsonb ? 'allowed_guild_ids'
		FROM channel WHERE id = $1`, previousGAAllowGroupDiscordChannelID).Scan(&allowAllGuilds, &hasAllowedGuildIDsKey); err != nil {
		t.Fatalf("read migrated Discord allow_group channel config: %v", err)
	}
	if !allowAllGuilds {
		t.Fatalf("legacy Discord channel that relied on allow_group alone did not backfill allow_all_guilds=true")
	}
	if hasAllowedGuildIDsKey {
		t.Fatalf("Discord allow_group backfill unexpectedly added an allowed_guild_ids key; it must only set allow_all_guilds")
	}
	var legacyActorType string
	if err := db.QueryRow(ctx, `SELECT actor_type FROM ctx_message WHERE id = $1`, previousGAMessageID).Scan(&legacyActorType); err != nil {
		t.Fatalf("read defaulted legacy message actor: %v", err)
	}
	if legacyActorType != "human" {
		t.Fatalf("defaulted legacy message actor=%q, want human", legacyActorType)
	}
	// The plugin scheduler columns are gone, the orphaned plugin-owned row was
	// purged, and the user subscription kept its template key.
	if got := count("dropped plugin scheduler columns", `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'sched_job' AND column_name IN ('plugin_id', 'runtime_name')`); got != 0 {
		t.Fatalf("sched_job plugin scheduler columns = %d, want 0", got)
	}
	if got := count("purged plugin-owned scheduler rows", `SELECT count(*) FROM sched_job WHERE owner_kind = 'plugin'`); got != 0 {
		t.Fatalf("plugin-owned sched_job rows = %d, want 0", got)
	}
	var survivingJobKey string
	if err := db.QueryRow(ctx, `SELECT job_key FROM sched_job WHERE id = $1`, previousGAUserJobID).Scan(&survivingJobKey); err != nil {
		t.Fatalf("read surviving user subscription job: %v", err)
	}
	if survivingJobKey != "digest" {
		t.Fatalf("surviving subscription job_key = %q, want digest", survivingJobKey)
	}
	if got := count("rebuilt scheduler owner index", `SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND tablename = 'sched_job' AND indexname = 'idx_sched_job_owner' AND indexdef LIKE '%owner_kind, job_key%'`); got != 1 {
		t.Fatalf("rebuilt idx_sched_job_owner = %d, want 1", got)
	}
	if got := count("session inbox table", `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'ctx_session_inbox'`); got != 1 {
		t.Fatalf("session inbox tables = %d, want 1", got)
	}
	if got := count("message inbox column", `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'ctx_message' AND column_name = 'inbox_id'`); got != 1 {
		t.Fatalf("message inbox columns = %d, want 1", got)
	}
	if got := count("message inbox unique index", `SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND tablename = 'ctx_message' AND indexname = 'idx_ctx_message_inbox_id'`); got != 1 {
		t.Fatalf("message inbox indexes = %d, want 1", got)
	}
	for _, tc := range []struct {
		name, messageID string
	}{
		{name: "delegate", messageID: previousGADelegateMsgID},
		{name: "scheduler", messageID: previousGASchedulerMsgID},
		{name: "task", messageID: previousGATaskMsgID},
	} {
		var actorType string
		var actorID pgtype.Text
		if err := db.QueryRow(ctx, `SELECT actor_type, actor_id FROM ctx_message WHERE id = $1`, tc.messageID).Scan(&actorType, &actorID); err != nil {
			t.Fatalf("read legacy %s actor: %v", tc.name, err)
		}
		if actorType != "human" {
			t.Fatalf("legacy %s actor=%q, want conservative human default", tc.name, actorType)
		}
		if actorID.Valid {
			t.Fatalf("legacy %s actor ID=%#v, want NULL without row-level provenance", tc.name, actorID)
		}
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO auth_provisioned_user (id, external_id, user_id, created_by_user_id, created_by_token_id)
		VALUES ('00000000-0000-0000-0000-000000000015', 'previous-ga-external', $1, $1, '00000000-0000-0000-0000-000000000014')`, previousGAUserID); err != nil {
		t.Fatalf("insert provisioned-user migration fixture: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE personal_access_token SET issued_by_token_id = id, issued_by_provisioning = true WHERE public_id = 'previous-ga-pat'`); err != nil {
		t.Fatalf("set provisioned PAT issuer migration fixture: %v", err)
	}
	if got := count("provisioned-user migration fixture", `SELECT count(*) FROM auth_provisioned_user WHERE external_id = 'previous-ga-external'`); got != 1 {
		t.Fatalf("provisioned-user rows = %d, want 1", got)
	}
	if got := count("retired vault entries", `SELECT count(*) FROM vault_entry WHERE name IN ('LARK_CLI_OAUTH', 'FEISHU_CLI_OAUTH')`); got != 0 {
		t.Fatalf("retired vault entries = %d, want 0", got)
	}
	if got := count("custom vault entry", `SELECT count(*) FROM vault_entry WHERE name = 'CUSTOM_SECRET'`); got != 1 {
		t.Fatalf("custom vault entries = %d, want 1", got)
	}
	if got := count("retired OAuth providers", `SELECT count(*) FROM plugin_oauth_provider WHERE provider_id IN ('lark', 'feishu')`); got != 0 {
		t.Fatalf("retired OAuth providers = %d, want 0", got)
	}
	if got := count("custom OAuth provider", `SELECT count(*) FROM plugin_oauth_provider WHERE provider_id = 'custom'`); got != 1 {
		t.Fatalf("custom OAuth providers = %d, want 1", got)
	}
	var larkClean, larkSparse, larkSnapshotPreserved, larkCustom, unrelatedPreserved bool
	if err := db.QueryRow(ctx, `
		SELECT NOT (config::jsonb ? 'oauth_provider') AND NOT (config::jsonb ? 'session_env'),
		       config::jsonb @> '{"$sparse": true}',
		       config::jsonb->'name' = 'null'::jsonb AND config::jsonb->'binaries' = 'null'::jsonb,
		       config::jsonb->>'prompt' = 'custom prompt' AND config::jsonb->>'custom' = 'keep' AND session_env_vault_key = 'custom-vault-key',
		       EXISTS (SELECT 1 FROM plugin_override WHERE plugin_id = 'tool/custom' AND enabled = false AND config::jsonb->>'custom' = 'untouched')
		FROM plugin_override WHERE plugin_id = 'tool/lark-cli'`).Scan(&larkClean, &larkSparse, &larkSnapshotPreserved, &larkCustom, &unrelatedPreserved); err != nil {
		t.Fatalf("read cleaned Lark override: %v", err)
	}
	if !larkClean || !larkSparse || !larkSnapshotPreserved || !larkCustom || !unrelatedPreserved {
		t.Fatalf("Lark cleanup = fields removed %v, sparse %v, snapshot preserved %v, custom preserved %v, unrelated preserved %v; want all true", larkClean, larkSparse, larkSnapshotPreserved, larkCustom, unrelatedPreserved)
	}
	if got := count("deleted sandbox plugins", `SELECT count(*) FROM plugin WHERE id LIKE 'sandbox/%'`); got != 0 {
		t.Fatalf("sandbox plugin rows = %d, want 0", got)
	}
	if got := count("deleted sandbox plugin state", `SELECT count(*) FROM plugin_state WHERE plugin_id LIKE 'sandbox/%'`); got != 0 {
		t.Fatalf("sandbox plugin state rows = %d, want 0", got)
	}
	if got := count("retired RTK rows", `
		SELECT (SELECT count(*) FROM plugin WHERE id = 'hook/rtk') +
		       (SELECT count(*) FROM plugin_state WHERE plugin_id = 'hook/rtk') +
		       (SELECT count(*) FROM plugin_override WHERE plugin_id = 'hook/rtk')`); got != 0 {
		t.Fatalf("retired RTK rows = %d, want 0", got)
	}
	if got := count("retired tap-web rows", `
		SELECT (SELECT count(*) FROM plugin WHERE id = 'tool/tap-web') +
		       (SELECT count(*) FROM plugin_state WHERE plugin_id = 'tool/tap-web') +
		       (SELECT count(*) FROM plugin_override WHERE plugin_id = 'tool/tap-web')`); got != 0 {
		t.Fatalf("retired tap-web rows = %d, want 0", got)
	}
	if got := count("sandbox plugin near miss", `SELECT count(*) FROM plugin WHERE id = 'sandbox'`); got != 1 {
		t.Fatalf("sandbox plugin near-miss rows = %d, want 1", got)
	}
	if got := count("sandbox plugin state near miss", `SELECT count(*) FROM plugin_state WHERE plugin_id = 'sandbox' AND state_key = 'near-miss-state'`); got != 1 {
		t.Fatalf("sandbox plugin state near-miss rows = %d, want 1", got)
	}
	if got := count("unrelated plugin rows", `SELECT count(*) FROM plugin WHERE id = 'tool/unrelated'`); got != 1 {
		t.Fatalf("unrelated plugin rows = %d, want 1", got)
	}
	if got := count("unrelated plugin state", `SELECT count(*) FROM plugin_state WHERE plugin_id = 'tool/unrelated' AND state_key = 'unrelated-state'`); got != 1 {
		t.Fatalf("unrelated plugin state rows = %d, want 1", got)
	}

	var olderArchived, oldArchived, newArchived bool
	if err := db.QueryRow(ctx, `SELECT archived FROM ctx_conversation WHERE session_id = $1`, previousGAOlderSession).Scan(&olderArchived); err != nil {
		t.Fatalf("read older duplicate: %v", err)
	}
	if err := db.QueryRow(ctx, `SELECT archived FROM ctx_conversation WHERE session_id = $1`, previousGAOldSession).Scan(&oldArchived); err != nil {
		t.Fatalf("read archived duplicate: %v", err)
	}
	if err := db.QueryRow(ctx, `SELECT archived FROM ctx_conversation WHERE session_id = $1`, previousGANewSession).Scan(&newArchived); err != nil {
		t.Fatalf("read retained duplicate: %v", err)
	}
	if !olderArchived || !oldArchived || newArchived {
		t.Fatalf("duplicate archival = older %v, tie loser %v, winner %v; want true, true, false (last_active DESC then session_id DESC)", olderArchived, oldArchived, newArchived)
	}
	_, err := db.Exec(ctx, `INSERT INTO ctx_conversation (id, session_id, channel, kind, agent_id, user_id, group_id, last_active, created_at, updated_at) VALUES ('00000000-0000-0000-0000-000000000099', 'previous-ga-agent:group:duplicate', 'web', 'chat', $1, $2::text, $2::uuid, $3, $3, $3)`, previousGAAgentID, previousGAGroupID, previousGATime)
	assertConstraintViolation(t, err, "idx_one_agent_group_chat")
	if _, err := db.Exec(ctx, `INSERT INTO channel_chat_command_receipt (id, channel_id, chat_key, message_id, command, binding, created_at, updated_at) VALUES ('00000000-0000-0000-0000-000000000030', 'test-channel', 'test-chat', 'platform-message-1', '/new', $1, $2, $2)`, previousGAGroupID, previousGATime); err != nil {
		t.Fatalf("insert valid chat command receipt: %v", err)
	}
	_, err = db.Exec(ctx, `INSERT INTO channel_chat_command_receipt (id, channel_id, chat_key, message_id, command, binding) VALUES ('00000000-0000-0000-0000-000000000031', 'test-channel', 'test-chat', 'platform-message-1', '/new', $1)`, previousGAGroupID)
	assertConstraintViolation(t, err, "channel_chat_command_receipt_channel_id_chat_key_message_id_key")

	var mediaID *string
	if err := db.QueryRow(ctx, `SELECT media_id::text FROM ctx_message_part WHERE id = $1`, previousGAPartID).Scan(&mediaID); err != nil {
		t.Fatalf("read legacy message part: %v", err)
	}
	if mediaID != nil {
		t.Fatalf("legacy message part media_id = %q, want NULL", *mediaID)
	}

	if _, err := db.Exec(ctx, `INSERT INTO webhook (id, user_id, agent_id, name, provider, token_public_id, token_hash, token_last4, created_at, updated_at) VALUES ($1, $2, $3, 'valid', 'test', 'webhook-public-id', 'webhook-hash', '1234', $4, $4)`, previousGAWebhookID, previousGAUserID, previousGAAgentID, previousGATime); err != nil {
		t.Fatalf("insert valid webhook: %v", err)
	}
	_, err = db.Exec(ctx, `INSERT INTO webhook (id, user_id, agent_id, name, provider, token_public_id, token_hash, token_last4) VALUES ('00000000-0000-0000-0000-000000000010', $1, 'missing-agent', 'invalid', 'test', 'other-public-id', 'hash', '1234')`, previousGAUserID)
	assertConstraintViolation(t, err, "webhook_agent_id_fkey")
	_, err = db.Exec(ctx, `INSERT INTO webhook (id, user_id, agent_id, name, provider, token_public_id, token_hash, token_last4) VALUES ('00000000-0000-0000-0000-000000000010', $1, $2, 'duplicate', 'test', 'webhook-public-id', 'hash', '1234')`, previousGAUserID, previousGAAgentID)
	assertConstraintViolation(t, err, "webhook_token_public_id_key")

	hash := make([]byte, 32)
	for i := range hash {
		hash[i] = byte(i)
	}
	if _, err := db.Exec(ctx, `INSERT INTO ctx_media (id, user_id, sha256, mime_type, size_bytes, created_at, updated_at) VALUES ($1, $2, $3, 'text/plain', 1, $4, $4)`, previousGAMediaID, previousGAUserID, hash, previousGATime); err != nil {
		t.Fatalf("insert valid media: %v", err)
	}
	// Media ownership is now a principal, not a user: the same content-addressed
	// row shape serves a group, uniqueness follows the generated owner, and a row
	// must name exactly one owner.
	_, err = db.Exec(ctx, `INSERT INTO ctx_media (id, user_id, sha256, mime_type, size_bytes) VALUES ('00000000-0000-0000-0000-000000000010', $1, $2, 'text/plain', 1)`, previousGAUserID, hash)
	assertConstraintViolation(t, err, "ctx_media_owner_sha256_key")
	if _, err := db.Exec(ctx, `INSERT INTO ctx_media (id, group_id, sha256, mime_type, size_bytes) VALUES ('00000000-0000-0000-0000-000000000011', $1, $2, 'text/plain', 1)`, previousGAGroupID, hash); err != nil {
		t.Fatalf("insert group-owned media: %v", err)
	}
	// The forward-migrated user row keeps its kind, and the same bytes under a
	// group are a separate row: identity is (owner_kind, owner_id, sha256).
	var kinds []string
	rows, err := db.Query(ctx, `SELECT owner_kind FROM ctx_media WHERE sha256 = $1 ORDER BY owner_kind`, hash)
	if err != nil {
		t.Fatalf("read media owner kinds: %v", err)
	}
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatalf("scan media owner kind: %v", err)
		}
		kinds = append(kinds, kind)
	}
	rows.Close()
	if strings.Join(kinds, ",") != "group,user" {
		t.Fatalf("media owner kinds = %v, want one user row and one group row", kinds)
	}
	_, err = db.Exec(ctx, `INSERT INTO ctx_media (id, user_id, group_id, sha256, mime_type, size_bytes) VALUES ('00000000-0000-0000-0000-000000000012', $1, $2, $3, 'text/plain', 1)`, previousGAUserID, previousGAGroupID, hash)
	assertConstraintViolation(t, err, "ctx_media_owner_check")
	_, err = db.Exec(ctx, `INSERT INTO ctx_media (id, sha256, mime_type, size_bytes) VALUES ('00000000-0000-0000-0000-000000000013', $1, 'text/plain', 1)`, hash)
	assertConstraintViolation(t, err, "ctx_media_owner_check")
	_, err = db.Exec(ctx, `INSERT INTO ctx_media (id, user_id, sha256, mime_type, size_bytes) VALUES ('00000000-0000-0000-0000-000000000010', $1, $2, 'text/plain', 1)`, previousGAUserID, hash[:31])
	assertConstraintViolation(t, err, "ctx_media_sha256_check")
	// The baseline is now a column on the media row. A forward-migrated database
	// has nothing to backfill from (ctx_media postdates this GA boundary, so the
	// backfill itself is covered by ctx_media_baseline_migration_test.go); what
	// must hold here is the contract: absent means NULL, and "rendered to
	// nothing" is rejected rather than stored as a description.
	var mediaBaseline *string
	if err := db.QueryRow(ctx, `SELECT baseline FROM ctx_media WHERE id = $1`, previousGAMediaID).Scan(&mediaBaseline); err != nil {
		t.Fatalf("read media baseline: %v", err)
	}
	if mediaBaseline != nil {
		t.Fatalf("undescribed media baseline = %q, want NULL", *mediaBaseline)
	}
	_, err = db.Exec(ctx, `UPDATE ctx_media SET baseline = '' WHERE id = $1`, previousGAMediaID)
	assertConstraintViolation(t, err, "ctx_media_baseline_nonempty")
	if _, err := db.Exec(ctx, `UPDATE ctx_media SET baseline = 'described' WHERE id = $1`, previousGAMediaID); err != nil {
		t.Fatalf("store media baseline: %v", err)
	}

	if _, err := db.Exec(ctx, `UPDATE ctx_message_part SET media_id = $1 WHERE id = $2`, previousGAMediaID, previousGAPartID); err != nil {
		t.Fatalf("link message part to media: %v", err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM ctx_media WHERE id = $1`, previousGAMediaID); err != nil {
		t.Fatalf("delete linked media: %v", err)
	}
	if err := db.QueryRow(ctx, `SELECT media_id::text FROM ctx_message_part WHERE id = $1`, previousGAPartID).Scan(&mediaID); err != nil {
		t.Fatalf("read media-cleared part: %v", err)
	}
	if mediaID != nil {
		t.Fatalf("message part media_id after media delete = %q, want NULL", *mediaID)
	}

	if _, err := db.Exec(ctx, `INSERT INTO agent_provider_credential (agent_id, provider_id, api_key_enc, created_at, updated_at) VALUES ($1, $2, 'ciphertext', $3, $3)`, previousGACascadeAgentID, previousGAProviderID, previousGATime); err != nil {
		t.Fatalf("insert valid agent Provider credential: %v", err)
	}
	_, err = db.Exec(ctx, `INSERT INTO agent_provider_credential (agent_id, provider_id, api_key_enc) VALUES ($1, $2, 'duplicate-ciphertext')`, previousGACascadeAgentID, previousGAProviderID)
	assertConstraintViolation(t, err, "agent_provider_credential_pkey")
	_, err = db.Exec(ctx, `INSERT INTO agent_provider_credential (agent_id, provider_id, api_key_enc) VALUES ($1, $2, '')`, previousGAAgentID, previousGAProviderID)
	assertConstraintViolation(t, err, "agent_provider_credential_api_key_enc_check")
	_, err = db.Exec(ctx, `INSERT INTO agent_provider_credential (agent_id, provider_id, api_key_enc) VALUES ('missing-agent', $1, 'ciphertext')`, previousGAProviderID)
	assertConstraintViolation(t, err, "agent_provider_credential_agent_id_fkey")
	_, err = db.Exec(ctx, `INSERT INTO agent_provider_credential (agent_id, provider_id, api_key_enc) VALUES ($1, 'missing-provider', 'ciphertext')`, previousGACascadeAgentID)
	assertConstraintViolation(t, err, "agent_provider_credential_provider_id_fkey")
	if _, err := db.Exec(ctx, `DELETE FROM agent WHERE id = $1`, previousGACascadeAgentID); err != nil {
		t.Fatalf("delete credential Agent: %v", err)
	}
	if got := count("credentials after Agent cascade", `SELECT count(*) FROM agent_provider_credential WHERE agent_id = $1`, previousGACascadeAgentID); got != 0 {
		t.Fatalf("credential rows after Agent delete = %d, want 0", got)
	}
	if _, err := db.Exec(ctx, `INSERT INTO agent_provider_credential (agent_id, provider_id, api_key_enc, created_at, updated_at) VALUES ($1, $2, 'provider-ciphertext', $3, $3)`, previousGAAgentID, previousGAProviderID, previousGATime); err != nil {
		t.Fatalf("insert provider-cascade credential: %v", err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM provider WHERE id = $1`, previousGAProviderID); err != nil {
		t.Fatalf("delete credential Provider: %v", err)
	}
	if got := count("credentials after Provider cascade", `SELECT count(*) FROM agent_provider_credential WHERE provider_id = $1`, previousGAProviderID); got != 0 {
		t.Fatalf("credential rows after Provider delete = %d, want 0", got)
	}

	// Exercise durable channel guest identity and its active Agent conversation
	// after upgrading the previous GA database.
	if _, err := db.Exec(ctx, `
		INSERT INTO channel (id, name, type, agent_id, enabled, created_at, updated_at)
		VALUES ('previous-ga-discord', 'Previous GA Discord', 'discord', $1, true, $2, $2)
	`, previousGAAgentID, previousGATime); err != nil {
		t.Fatalf("insert Discord channel after previous-GA upgrade: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO channel_guest (id, channel_id, platform, external_id, created_at, updated_at)
		VALUES ($1, 'previous-ga-discord', 'discord', 'previous-ga-user', $2, $2)
	`, previousGAGuestID, previousGATime); err != nil {
		t.Fatalf("insert channel guest after previous-GA upgrade: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO ctx_conversation (
			id, session_id, channel, kind, agent_id, user_id, guest_id,
			last_active, created_at, updated_at
		) VALUES ($1, 'previous-ga-agent:guest:discord', 'discord', 'chat', $2, $3::text, $3::uuid, $4, $4, $4)
	`, previousGAGuestChatID, previousGAAgentID, previousGAGuestID, previousGATime); err != nil {
		t.Fatalf("insert guest conversation after previous-GA upgrade: %v", err)
	}
	if got := count("channel guest conversation", `
		SELECT count(*)
		FROM ctx_conversation AS conversation
		JOIN channel_guest AS guest ON guest.id = conversation.guest_id
		WHERE conversation.id = $1 AND guest.channel_id = 'previous-ga-discord'
	`, previousGAGuestChatID); got != 1 {
		t.Fatalf("channel guest conversations = %d, want 1", got)
	}
	if _, err := db.Exec(ctx, `INSERT INTO auth_user (id, email) VALUES ('00000000-0000-0000-0000-000000000048', 'other-user@test.invalid')`); err != nil {
		t.Fatalf("insert other user fixture: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO ctx_conversation (id, session_id, channel, kind, agent_id, user_id) VALUES
			('00000000-0000-0000-0000-000000000049', 'previous-ga-admin-own', 'web', 'chat', $1, $2),
			('00000000-0000-0000-0000-000000000050', 'previous-ga-other-user', 'web', 'chat', $1, '00000000-0000-0000-0000-000000000048')
	`, previousGAAgentID, previousGAUserID); err != nil {
		t.Fatalf("insert admin session visibility fixtures: %v", err)
	}
	adminSessions, err := sqlc.New(db).ListConversationsForAdminFiltered(ctx, sqlc.ListConversationsForAdminFilteredParams{
		AgentID: pgtype.Text{String: previousGAAgentID, Valid: true}, UserID: pgtype.Text{String: previousGAUserID, Valid: true},
		IncludeArchived: int32(1), ProjectIDIsNull: int32(0), Offset: 0, Limit: int32(-1),
	})
	if err != nil {
		t.Fatalf("list admin and guest sessions: %v", err)
	}
	var foundOwn, foundGuest, foundOther bool
	for _, conversation := range adminSessions {
		foundOwn = foundOwn || conversation.SessionID == "previous-ga-admin-own"
		foundGuest = foundGuest || conversation.ID == previousGAGuestChatID
		foundOther = foundOther || conversation.SessionID == "previous-ga-other-user"
	}
	if !foundOwn || !foundGuest || foundOther {
		t.Fatalf("admin session visibility: own=%v guest=%v other-user=%v, want true, true, false", foundOwn, foundGuest, foundOther)
	}
	_, err = db.Exec(ctx, `
		INSERT INTO ctx_conversation (
			id, session_id, channel, kind, agent_id, user_id, guest_id
		) VALUES ('00000000-0000-0000-0000-000000000046', 'previous-ga-agent:guest:duplicate', 'discord', 'chat', $1, $2::text, $2::uuid)
	`, previousGAAgentID, previousGAGuestID)
	assertConstraintViolation(t, err, "idx_one_agent_guest_chat")
	_, err = sqlc.New(db).CreateChannelGuest(ctx, sqlc.CreateChannelGuestParams{
		ID: "00000000-0000-0000-0000-000000000047", ChannelID: "previous-ga-discord",
		Platform: "discord", ExternalID: "over-cap", MaxGuests: 1,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("create channel guest above cap = %v, want no rows", err)
	}

	// Exercise the complete Library snapshot publication relationship after
	// upgrading the previous GA database, rather than checking table names only.
	if _, err := db.Exec(ctx, `
		INSERT INTO library_file (
			id, scope, user_id, agent_id, file_name, media_type,
			size_bytes, raw_sha256, status
		) VALUES ($1, 'user_agent', $2, $3, 'previous-ga.txt', 'text/plain', 1, $4, 'ready')
	`, previousGALibraryFile, previousGAUserID, previousGAAgentID, hash); err != nil {
		t.Fatalf("insert Library file after previous-GA upgrade: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO agent (id, name, workspace) VALUES ($1, 'Library Agent', '/tmp')
	`, previousGALibraryAgentID); err != nil {
		t.Fatalf("insert Library Agent after previous-GA upgrade: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO library_file (
			id, scope, agent_id, file_name, media_type,
			size_bytes, raw_sha256, status
		) VALUES ($1, 'system_agent', $2, 'agent-owned.txt', 'text/plain', 1, $3, 'ready')
	`, previousGAAgentLibraryFile, previousGALibraryAgentID, hash); err != nil {
		t.Fatalf("insert Agent-scoped Library file after previous-GA upgrade: %v", err)
	}
	_, err = db.Exec(ctx, `DELETE FROM agent WHERE id = $1`, previousGALibraryAgentID)
	assertConstraintViolation(t, err, "library_file_agent_id_fkey")
	if _, err := db.Exec(ctx, `
		INSERT INTO library_chunk_set (
			id, file_id, derivation_key, processor_key, raw_sha256, status
		) VALUES ($1, $2, 'previous-ga-derivation', 'previous-ga-processor', $3, 'building')
	`, previousGAChunkSet, previousGALibraryFile, hash); err != nil {
		t.Fatalf("insert Library ChunkSet after previous-GA upgrade: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO library_chunk (
			id, chunk_set_id, ordinal, content, content_sha256
		) VALUES ($1, $2, 0, 'previous GA library content', $3)
	`, previousGAChunk, previousGAChunkSet, hash); err != nil {
		t.Fatalf("insert Library chunk after previous-GA upgrade: %v", err)
	}
	var locatorDigestBytes int
	var locatorDigestMatches bool
	if err := db.QueryRow(ctx, `
		SELECT
			octet_length(locator_sha256),
			locator_sha256 = sha256(convert_to(locator::text, 'UTF8'))
		FROM library_chunk
		WHERE id = $1
	`, previousGAChunk).Scan(&locatorDigestBytes, &locatorDigestMatches); err != nil {
		t.Fatalf("read Library chunk locator digest after previous-GA upgrade: %v", err)
	}
	if locatorDigestBytes != sha256.Size || !locatorDigestMatches {
		t.Fatalf("Library chunk locator digest bytes = %d, matches locator = %t", locatorDigestBytes, locatorDigestMatches)
	}
	// Simulate an old worker submitting its content-only aggregate. The migration
	// trigger must replace it before the generation can become ready.
	if _, err := db.Exec(ctx, `
		UPDATE library_chunk_set
		SET status = 'ready', chunk_count = 1, content_digest = $1, completed_at = $2
		WHERE id = $3
	`, hash, previousGATime, previousGAChunkSet); err != nil {
		t.Fatalf("mark old-worker Library ChunkSet ready: %v", err)
	}
	var readyDigestMatches bool
	if err := db.QueryRow(ctx, `
		SELECT chunk_set.content_digest = sha256(decode(
			lpad(to_hex(chunk.ordinal), 16, '0') ||
			encode(chunk.content_sha256, 'hex') ||
			encode(chunk.locator_sha256, 'hex'),
			'hex'
		))
		FROM library_chunk_set AS chunk_set
		JOIN library_chunk AS chunk ON chunk.chunk_set_id = chunk_set.id
		WHERE chunk_set.id = $1
	`, previousGAChunkSet).Scan(&readyDigestMatches); err != nil {
		t.Fatalf("read old-worker Library ChunkSet digest: %v", err)
	}
	if !readyDigestMatches {
		t.Fatal("old-worker Library ChunkSet became ready without locator-aware integrity")
	}
	if _, err := db.Exec(ctx, `
		UPDATE library_file SET active_chunk_set_id = $1 WHERE id = $2
	`, previousGAChunkSet, previousGALibraryFile); err != nil {
		t.Fatalf("publish Library ChunkSet after previous-GA upgrade: %v", err)
	}
	if got := count("published Library chunks", `
		SELECT count(*)
		FROM library_file AS file
		JOIN library_chunk_set AS chunk_set ON chunk_set.id = file.active_chunk_set_id
		JOIN library_chunk AS chunk ON chunk.chunk_set_id = chunk_set.id
		WHERE file.id = $1
	`, previousGALibraryFile); got != 1 {
		t.Fatalf("published Library chunks = %d, want 1", got)
	}

	var lastTurnStartedAt, lastTurnCompletedAt, lastViewedAt pgtype.Timestamptz
	var lastTurnResult pgtype.Text
	if err := db.QueryRow(ctx, `
		SELECT last_turn_started_at, last_turn_completed_at, last_turn_result, last_viewed_at
		FROM ctx_conversation
		WHERE id = $1
	`, previousGANewChatID).Scan(&lastTurnStartedAt, &lastTurnCompletedAt, &lastTurnResult, &lastViewedAt); err != nil {
		t.Fatalf("read migrated session activity: %v", err)
	}
	if lastTurnStartedAt.Valid || lastTurnCompletedAt.Valid || lastTurnResult.Valid || lastViewedAt.Valid {
		t.Fatalf("migrated session activity = %v/%v/%v/%v, want all null", lastTurnStartedAt, lastTurnCompletedAt, lastTurnResult, lastViewedAt)
	}

	if _, err := db.Exec(ctx, `UPDATE channel_guest SET updated_at = now() - interval '31 days' WHERE id = $1`, previousGAGuestID); err != nil {
		t.Fatalf("age guest activity: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE channel SET config = '{"guest_retention_days":365}' WHERE id = 'previous-ga-discord'`); err != nil {
		t.Fatalf("configure guest retention: %v", err)
	}
	queries := sqlc.New(db)
	deleted, err := queries.PurgeExpiredChannelGuest(ctx)
	if err != nil {
		t.Fatalf("purge expired channel guests: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("guest retention purge deleted %d guests before configured retention, want 0", deleted)
	}
	if _, err := db.Exec(ctx, `UPDATE channel SET config = '{' WHERE id = 'previous-ga-discord'`); err != nil {
		t.Fatalf("set malformed channel config: %v", err)
	}
	deleted, err = queries.PurgeExpiredChannelGuest(ctx)
	if err != nil {
		t.Fatalf("purge expired channel guests with malformed config: %v", err)
	}
	if deleted != 1 || count("retained expired guest conversations", `SELECT count(*) FROM ctx_conversation WHERE guest_id = $1`, previousGAGuestID) != 0 {
		t.Fatalf("guest retention purge deleted %d guests without cascading conversations, want 1 guest and 0 conversations", deleted)
	}

	if _, err := db.Exec(ctx, `
		INSERT INTO agent_llm_call (
			session_id, agent_id, provider, model, usage_reported,
			input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			cost_usd, duration_ms, occurred_at
		) VALUES ($1, $2, 'previous-ga-provider', 'previous-ga-model', true, 10, 5, 3, 2, 0.0125, 100, $3)
	`, previousGANewSession, previousGAAgentID, previousGATime); err != nil {
		t.Fatalf("write migrated LLM usage row: %v", err)
	}
	var usageInput int64
	var usageCost pgtype.Numeric
	if err := db.QueryRow(ctx, `SELECT input_tokens, cost_usd FROM agent_llm_call WHERE session_id = $1`, previousGANewSession).Scan(&usageInput, &usageCost); err != nil {
		t.Fatalf("read migrated LLM usage row: %v", err)
	}
	if usageInput != 10 || !usageCost.Valid {
		t.Fatalf("migrated LLM usage row = input %d / cost %+v, want 10 / priced", usageInput, usageCost)
	}

	if got := count("group history BM25 index", `SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND tablename = 'ctx_group_message' AND indexname = 'idx_ctx_group_message_bm25'`); got != 1 {
		t.Fatalf("group history BM25 indexes = %d, want 1", got)
	}

	var removedGroupMemoryTable bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('public.ctx_group_memory') IS NULL`).Scan(&removedGroupMemoryTable); err != nil {
		t.Fatalf("check removed group memory table: %v", err)
	}
	if !removedGroupMemoryTable {
		t.Fatal("group memory table remains after upgrade")
	}

	// The two legacy model surfaces must be gone, replaced by one empty
	// default_models row an admin fills in. Nothing is carried over on purpose:
	// neither legacy value named the provider it belonged to, and inferring one
	// for embedding would file new vectors into an existing space under a
	// different account's model.
	var unifiedModels string
	if err := db.QueryRow(ctx, `SELECT value FROM app_setting WHERE key = 'default_models'`).Scan(&unifiedModels); err != nil {
		t.Fatalf("read unified default models: %v", err)
	}
	if unifiedModels != "{}" {
		t.Fatalf("migrated default models = %s, want an empty setting", unifiedModels)
	}
	if got := count("legacy vision setting rows", `SELECT count(*) FROM app_setting WHERE key = 'vision'`); got != 0 {
		t.Fatalf("legacy vision setting rows = %d, want 0", got)
	}

	// The embedding row survives, stripped of the model and inline credentials
	// that now live in default_models and the provider catalog.
	var laneEnabled, laneNormalize bool
	var laneDim int
	var laneModel, laneKey *string
	if err := db.QueryRow(ctx, `
		SELECT (value::jsonb ->> 'enabled')::bool, (value::jsonb ->> 'dim')::int,
		       (value::jsonb ->> 'normalize')::bool,
		       value::jsonb ->> 'model', value::jsonb ->> 'api_key'
		FROM app_setting WHERE key = 'embedding'`).
		Scan(&laneEnabled, &laneDim, &laneNormalize, &laneModel, &laneKey); err != nil {
		t.Fatalf("read embedding lane settings: %v", err)
	}
	if !laneEnabled || laneDim != 1536 || !laneNormalize {
		t.Fatalf("embedding lane knobs = enabled:%v dim:%d normalize:%v, want them preserved", laneEnabled, laneDim, laneNormalize)
	}
	if laneModel != nil || laneKey != nil {
		t.Fatalf("embedding row still carries model/api_key (%v/%v), want them stripped", laneModel, laneKey)
	}

	var latest int64
	if err := db.QueryRow(ctx, `SELECT version_id FROM goose_db_version ORDER BY id DESC LIMIT 1`).Scan(&latest); err != nil {
		t.Fatalf("read goose migration ledger: %v", err)
	}
	if latest != latestMigrationVersion {
		t.Fatalf("latest goose migration = %d, want %d", latest, latestMigrationVersion)
	}
}

func assertConstraintViolation(t *testing.T, err error, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("constraint %s: got error %v, want PostgreSQL constraint violation", constraint, err)
	}
	if pgErr.ConstraintName != constraint {
		t.Fatalf("constraint violation = %q (%s), want %q", pgErr.ConstraintName, pgErr.Code, constraint)
	}
}
