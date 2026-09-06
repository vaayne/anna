package db

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	mediaBaselineBeforeMigration = 90000000000029
	mediaBaselineMigration       = 90000000000030
)

// The baseline moved from the message block onto the ctx_media row it describes.
// Both legacy homes have to arrive: a direct session kept it in the image part's
// text projection, a group session inside the content_blocks JSONB. The rows that
// must stay NULL are the point of the last two assertions — the "unavailable"
// marker is a rendering failure, and a media object nobody ever described has no
// baseline to inherit.
func TestCtxMediaBaselineBackfillsBothLegacyHomes(t *testing.T) {
	db, provider := newTestDBAtMigration(t, mediaBaselineBeforeMigration)
	ctx := t.Context()

	userID := seedMediaUser(t, db)
	groupID := seedMediaGroup(t, db)
	dmMedia := seedUserMedia(t, db, userID, 1)
	unavailableMedia := seedUserMedia(t, db, userID, 2)
	untouchedMedia := seedUserMedia(t, db, userID, 3)
	malformedMedia := seedUserMedia(t, db, userID, 5)
	emptySectionMedia := seedUserMedia(t, db, userID, 6)
	extraSectionMedia := seedUserMedia(t, db, userID, 7)
	groupMedia := seedGroupMedia(t, db, groupID, 4)
	malformedGroupMedia := seedGroupMedia(t, db, groupID, 8)

	const dmBaseline = "## Text\nreceipt\n\n## Scene\na paper receipt on a desk"
	const groupBaseline = "## Text\nwhiteboard\n\n## Scene\na whiteboard covered in arrows"

	conversationID := seedMediaConversation(t, db, userID)
	messageID := seedMediaMessage(t, db, conversationID, 1)
	seedMediaPart(t, db, messageID, 0, dmMedia, dmBaseline)
	seedMediaPart(t, db, messageID, 1, unavailableMedia, "[Image baseline unavailable.]")
	seedMediaPart(t, db, messageID, 2, untouchedMedia, "")
	// Non-empty text that is not a baseline. Adopting it would be worse than
	// adopting nothing: the reader rejects it, and the write-once column would
	// then refuse every real render for the life of the row.
	seedMediaPart(t, db, messageID, 3, malformedMedia, "just some prose about the picture")
	seedMediaPart(t, db, messageID, 4, emptySectionMedia, "## Text\n\n\n## Scene\na scene with no transcription")
	seedMediaPart(t, db, messageID, 5, extraSectionMedia, "## Text\nhi\n\n## Scene\na scene\n\n## Notes\nsomething else")

	seedGroupMessageBlocks(t, db, groupID, 1, `[
		{"kind":"text","text":"look at this"},
		{"kind":"image_ref","media_id":`+jsonString(groupMedia)+`,"baseline":`+jsonString(groupBaseline)+`}
	]`)
	// A malformed id in a legacy row must be skipped, not abort the migration.
	seedGroupMessageBlocks(t, db, groupID, 2, `[{"kind":"image_ref","media_id":"not-a-uuid","baseline":"junk"}]`)
	// The group side applies the same contract to the block's baseline.
	seedGroupMessageBlocks(t, db, groupID, 3, `[{"kind":"image_ref","media_id":`+jsonString(malformedGroupMedia)+`,"baseline":"a description, but not a baseline"}]`)

	if _, err := provider.UpTo(ctx, mediaBaselineMigration); err != nil {
		t.Fatalf("migrate ctx_media baseline: %v", err)
	}

	assertMediaBaseline(t, db, dmMedia, dmBaseline)
	assertMediaBaseline(t, db, groupMedia, groupBaseline)
	assertMediaBaselineNull(t, db, unavailableMedia)
	assertMediaBaselineNull(t, db, untouchedMedia)
	assertMediaBaselineNull(t, db, malformedMedia)
	assertMediaBaselineNull(t, db, emptySectionMedia)
	assertMediaBaselineNull(t, db, extraSectionMedia)
	assertMediaBaselineNull(t, db, malformedGroupMedia)
}

func seedMediaUser(t *testing.T, db *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := db.QueryRow(context.Background(),
		`INSERT INTO auth_user (email) VALUES ('baseline@example.com') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func seedMediaGroup(t *testing.T, db *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := db.QueryRow(context.Background(),
		`INSERT INTO ctx_group_state (platform, platform_group_id) VALUES ('telegram', 'g-1') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	return id
}

func seedUserMedia(t *testing.T, db *pgxpool.Pool, userID string, marker byte) string {
	t.Helper()
	var id string
	if err := db.QueryRow(context.Background(),
		`INSERT INTO ctx_media (user_id, sha256, mime_type, size_bytes)
		 VALUES ($1, $2, 'image/png', 1) RETURNING id`, userID, mediaDigest(marker)).Scan(&id); err != nil {
		t.Fatalf("seed user media: %v", err)
	}
	return id
}

func seedGroupMedia(t *testing.T, db *pgxpool.Pool, groupID string, marker byte) string {
	t.Helper()
	var id string
	if err := db.QueryRow(context.Background(),
		`INSERT INTO ctx_media (group_id, sha256, mime_type, size_bytes)
		 VALUES ($1, $2, 'image/png', 1) RETURNING id`, groupID, mediaDigest(marker)).Scan(&id); err != nil {
		t.Fatalf("seed group media: %v", err)
	}
	return id
}

func mediaDigest(marker byte) []byte {
	digest := make([]byte, 32)
	digest[0] = marker
	return digest
}

func seedMediaConversation(t *testing.T, db *pgxpool.Pool, userID string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(context.Background(),
		`INSERT INTO ctx_conversation (session_id, user_id) VALUES ('sess-baseline', $1) RETURNING id`,
		userID).Scan(&id); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	return id
}

func seedMediaMessage(t *testing.T, db *pgxpool.Pool, conversationID string, seq int64) string {
	t.Helper()
	var id string
	if err := db.QueryRow(context.Background(),
		`INSERT INTO ctx_message (conversation_id, seq, role, content, token_count)
		 VALUES ($1, $2, 'user', '', 0) RETURNING id`, conversationID, seq).Scan(&id); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	return id
}

func seedMediaPart(t *testing.T, db *pgxpool.Pool, messageID string, ordinal int64, mediaID, text string) {
	t.Helper()
	if _, err := db.Exec(context.Background(),
		`INSERT INTO ctx_message_part (message_id, part_type, ordinal, text_content, media_id)
		 VALUES ($1, 'image', $2, $3, $4)`, messageID, ordinal, text, mediaID); err != nil {
		t.Fatalf("seed message part: %v", err)
	}
}

func seedGroupMessageBlocks(t *testing.T, db *pgxpool.Pool, groupID string, seq int64, blocks string) {
	t.Helper()
	if _, err := db.Exec(context.Background(),
		`INSERT INTO ctx_group_message (group_id, seq, actor_type, actor_id, content_blocks)
		 VALUES ($1, $2, 'user', 'u-1', $3::jsonb)`, groupID, seq, blocks); err != nil {
		t.Fatalf("seed group message: %v", err)
	}
}

func jsonString(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func readMediaBaseline(t *testing.T, db *pgxpool.Pool, mediaID string) pgtype.Text {
	t.Helper()
	var baseline pgtype.Text
	if err := db.QueryRow(context.Background(),
		`SELECT baseline FROM ctx_media WHERE id = $1`, mediaID).Scan(&baseline); err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	return baseline
}

func assertMediaBaseline(t *testing.T, db *pgxpool.Pool, mediaID, want string) {
	t.Helper()
	got := readMediaBaseline(t, db, mediaID)
	if !got.Valid || got.String != want {
		t.Fatalf("media %s baseline = %#v, want %q", mediaID, got, want)
	}
}

func assertMediaBaselineNull(t *testing.T, db *pgxpool.Pool, mediaID string) {
	t.Helper()
	if got := readMediaBaseline(t, db, mediaID); got.Valid {
		t.Fatalf("media %s baseline = %q, want NULL", mediaID, got.String)
	}
}
