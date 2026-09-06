package db

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	discordExplicitGroupAccessBeforeMigration = 90000000000017
	discordExplicitGroupAccessMigration       = 90000000000018
)

func TestDiscordExplicitGroupAccessBackfillsAllowAllGuilds(t *testing.T) {
	db, provider := newTestDBAtMigration(t, discordExplicitGroupAccessBeforeMigration)
	ctx := t.Context()

	seed := func(id, configJSON string) {
		t.Helper()
		if _, err := db.Exec(ctx, `
			INSERT INTO channel (id, name, type, enabled, config)
			VALUES ($1, $1, 'discord', true, $2)
		`, id, configJSON); err != nil {
			t.Fatalf("seed channel %s: %v", id, err)
		}
	}
	seed("group-enabled", `{"token":"t","allow_group":true}`)
	seed("group-disabled", `{"token":"t","allow_group":false}`)
	seed("already-explicit-false", `{"token":"t","allow_group":true,"allow_all_guilds":false}`)
	seed("already-explicit-true", `{"token":"t","allow_group":true,"allow_all_guilds":true}`)
	if _, err := db.Exec(ctx, `
		INSERT INTO channel (id, name, type, enabled, config)
		VALUES ('telegram-group', 'telegram-group', 'telegram', true, '{"token":"t","allow_group":true}')
	`); err != nil {
		t.Fatalf("seed telegram channel: %v", err)
	}

	if _, err := provider.UpTo(ctx, discordExplicitGroupAccessMigration); err != nil {
		t.Fatalf("run discord explicit group access migration: %v", err)
	}

	for _, tc := range []struct {
		id      string
		present bool
		want    bool
	}{
		{"group-enabled", true, true},
		{"group-disabled", false, false},
		{"already-explicit-false", true, false},
		{"already-explicit-true", true, true},
	} {
		decoded := readChannelConfig(t, ctx, db, tc.id)
		got, ok := decoded["allow_all_guilds"]
		if ok != tc.present {
			t.Errorf("%s: allow_all_guilds present = %v, want %v (value %v)", tc.id, ok, tc.present, got)
			continue
		}
		if tc.present && got != tc.want {
			t.Errorf("%s: allow_all_guilds = %v, want %v", tc.id, got, tc.want)
		}
	}

	var telegramConfig string
	if err := db.QueryRow(ctx, `SELECT config FROM channel WHERE id = 'telegram-group'`).Scan(&telegramConfig); err != nil {
		t.Fatalf("read telegram channel: %v", err)
	}
	if telegramConfig != `{"token":"t","allow_group":true}` {
		t.Errorf("non-discord channel config mutated = %s", telegramConfig)
	}
}

func TestDiscordExplicitGroupAccessDownIsNoOp(t *testing.T) {
	// Down cannot tell a backfilled allow_all_guilds value apart from one an
	// operator set deliberately afterward (see the migration's Up/Down
	// comments), so rollback leaves every config's guild-access policy
	// exactly as it already evaluates — nothing is removed or rewritten.
	db, provider := newTestDBAtMigration(t, discordExplicitGroupAccessBeforeMigration)
	ctx := t.Context()

	if _, err := provider.UpTo(ctx, discordExplicitGroupAccessMigration); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	const seededConfig = `{"token":"t","allow_group":true,"allow_all_guilds":true,"allowed_guild_ids":["g1"]}`
	if _, err := db.Exec(ctx, `
		INSERT INTO channel (id, name, type, enabled, config)
		VALUES ('rollback-target', 'rollback-target', 'discord', true, $1)
	`, seededConfig); err != nil {
		t.Fatalf("seed channel: %v", err)
	}

	if _, err := provider.DownTo(ctx, discordExplicitGroupAccessMigration-1); err != nil {
		t.Fatalf("roll back migration: %v", err)
	}

	var config string
	if err := db.QueryRow(ctx, `SELECT config FROM channel WHERE id = 'rollback-target'`).Scan(&config); err != nil {
		t.Fatalf("read channel: %v", err)
	}
	if config != seededConfig {
		t.Errorf("config changed by no-op Down: got %s, want %s", config, seededConfig)
	}
}

func readChannelConfig(t *testing.T, ctx context.Context, db *pgxpool.Pool, id string) map[string]any {
	t.Helper()
	var config string
	if err := db.QueryRow(ctx, `SELECT config FROM channel WHERE id = $1`, id).Scan(&config); err != nil {
		t.Fatalf("read channel %s: %v", id, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(config), &decoded); err != nil {
		t.Fatalf("decode channel %s config: %v", id, err)
	}
	return decoded
}
