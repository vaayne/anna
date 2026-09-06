package db

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	reflectWatermarkBeforeMigration = 20260716090000
	reflectWatermarkMigration       = 20260720091113
)

func TestMigrateReflectLineWatermarks(t *testing.T) {
	db, provider := newTestDBAtMigration(t, reflectWatermarkBeforeMigration)
	ctx := t.Context()

	seedReflectState(t, db, "legacy-only", "review_watermark", `{"reviewed_at":"2026-07-01 10:00:00"}`)
	seedReflectState(t, db, "line-newer", "review_watermark", `{"reviewed_at":"2026-07-01 10:00:00"}`)
	seedReflectState(t, db, "line-newer", "reflect_watermark:fact", `{"reviewed_at":"2026-07-01T11:00:00Z","reviewed_seq":11,"marker":"keep"}`)
	seedReflectState(t, db, "legacy-newer", "review_watermark", `{"reviewed_at":"2026-07-01 12:00:00"}`)
	seedReflectState(t, db, "legacy-newer", "reflect_watermark:fact", `{"reviewed_at":"2026-07-01T11:00:00Z","reviewed_seq":42}`)
	seedReflectState(t, db, "legacy-newer", "reflect_watermark:skill", `{"reviewed_at":"2026-07-01T13:00:00Z","reviewed_seq":43}`)
	seedReflectState(t, db, "equal", "review_watermark", `{"reviewed_at":"2026-07-01 12:00:00"}`)
	seedReflectState(t, db, "equal", "reflect_watermark:fact", `{"reviewed_at":"2026-07-01T12:00:00Z","reviewed_seq":7}`)
	seedReflectState(t, db, "empty", "review_watermark", `{}`)
	seedReflectState(t, db, "empty", "reflect_watermark:fact", `{"reviewed_at":"2026-07-01T13:00:00Z","reviewed_seq":8}`)
	seedReflectState(t, db, "line-only", "reflect_watermark:fact", `{"reviewed_at":"2026-07-01T14:00:00Z","reviewed_seq":9}`)

	if _, err := provider.UpTo(ctx, reflectWatermarkMigration); err != nil {
		t.Fatalf("migrate reflect watermarks: %v", err)
	}

	assertReflectState(t, db, "legacy-only", "review_watermark", `{"reviewed_at":"2026-07-01 10:00:00"}`)
	assertReflectState(t, db, "legacy-only", "reflect_watermark:fact", `{"reviewed_at":"2026-07-01 10:00:00"}`)
	assertReflectState(t, db, "legacy-only", "reflect_watermark:skill", `{"reviewed_at":"2026-07-01 10:00:00"}`)
	assertReflectState(t, db, "line-newer", "reflect_watermark:fact", `{"reviewed_at":"2026-07-01T11:00:00Z","reviewed_seq":11,"marker":"keep"}`)
	assertReflectState(t, db, "line-newer", "reflect_watermark:skill", `{"reviewed_at":"2026-07-01 10:00:00"}`)
	assertReflectState(t, db, "legacy-newer", "reflect_watermark:fact", `{"reviewed_at":"2026-07-01 12:00:00"}`)
	assertReflectState(t, db, "legacy-newer", "reflect_watermark:skill", `{"reviewed_at":"2026-07-01T13:00:00Z","reviewed_seq":43}`)
	assertReflectState(t, db, "equal", "reflect_watermark:fact", `{"reviewed_at":"2026-07-01T12:00:00Z","reviewed_seq":7}`)
	assertReflectState(t, db, "empty", "reflect_watermark:fact", `{"reviewed_at":"2026-07-01T13:00:00Z","reviewed_seq":8}`)
	assertReflectState(t, db, "empty", "reflect_watermark:skill", `{}`)
	assertReflectState(t, db, "line-only", "reflect_watermark:fact", `{"reviewed_at":"2026-07-01T14:00:00Z","reviewed_seq":9}`)
	assertReflectStateMissing(t, db, "line-only", "reflect_watermark:skill")

	// Re-applying the already-applied migration must leave migrated values
	// unchanged instead of stripping sequence information from newer lines.
	if _, err := provider.UpTo(ctx, reflectWatermarkMigration); err != nil {
		t.Fatalf("repeat watermark migration: %v", err)
	}
	assertReflectState(t, db, "line-newer", "reflect_watermark:fact", `{"reviewed_at":"2026-07-01T11:00:00Z","reviewed_seq":11,"marker":"keep"}`)
	assertReflectState(t, db, "legacy-newer", "reflect_watermark:fact", `{"reviewed_at":"2026-07-01 12:00:00"}`)
}

func TestMigrateReflectLineWatermarksRejectsMalformedTimestamp(t *testing.T) {
	db, provider := newTestDBAtMigration(t, reflectWatermarkBeforeMigration)
	ctx := t.Context()
	seedReflectState(t, db, "valid", "review_watermark", `{"reviewed_at":"2026-07-01 10:00:00"}`)
	seedReflectState(t, db, "invalid", "review_watermark", `{"reviewed_at":"not-a-timestamp"}`)

	if _, err := provider.UpTo(ctx, reflectWatermarkMigration); err == nil {
		t.Fatal("migration accepted a malformed Reflect watermark")
	}
	assertReflectStateMissing(t, db, "valid", "reflect_watermark:fact")
	assertReflectStateMissing(t, db, "valid", "reflect_watermark:skill")
	assertReflectState(t, db, "invalid", "review_watermark", `{"reviewed_at":"not-a-timestamp"}`)
}

func seedReflectState(t *testing.T, db *pgxpool.Pool, sessionID, stateKey, value string) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `
		INSERT INTO plugin_state (plugin_id, scope_kind, scope_id, state_key, value)
		VALUES ('reflect', 'session', $1, $2, $3::jsonb)
	`, sessionID, stateKey, value); err != nil {
		t.Fatalf("seed Reflect state %s/%s: %v", sessionID, stateKey, err)
	}
}

func assertReflectState(t *testing.T, db *pgxpool.Pool, sessionID, stateKey, wantJSON string) {
	t.Helper()
	var gotJSON string
	if err := db.QueryRow(context.Background(), `
		SELECT value::text
		FROM plugin_state
		WHERE plugin_id = 'reflect'
		  AND scope_kind = 'session'
		  AND scope_id = $1
		  AND state_key = $2
	`, sessionID, stateKey).Scan(&gotJSON); err != nil {
		t.Fatalf("read Reflect state %s/%s: %v", sessionID, stateKey, err)
	}
	var got, want any
	if err := json.Unmarshal([]byte(gotJSON), &got); err != nil {
		t.Fatalf("decode Reflect state %s/%s: %v", sessionID, stateKey, err)
	}
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatalf("decode expected Reflect state %s/%s: %v", sessionID, stateKey, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Reflect state %s/%s = %s, want %s", sessionID, stateKey, gotJSON, wantJSON)
	}
}

func assertReflectStateMissing(t *testing.T, db *pgxpool.Pool, sessionID, stateKey string) {
	t.Helper()
	var count int
	if err := db.QueryRow(context.Background(), `
		SELECT count(*)
		FROM plugin_state
		WHERE plugin_id = 'reflect'
		  AND scope_kind = 'session'
		  AND scope_id = $1
		  AND state_key = $2
	`, sessionID, stateKey).Scan(&count); err != nil {
		t.Fatalf("count Reflect state %s/%s: %v", sessionID, stateKey, err)
	}
	if count != 0 {
		t.Fatalf("Reflect state %s/%s exists", sessionID, stateKey)
	}
}
