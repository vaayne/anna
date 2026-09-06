package db

import (
	"context"
	"testing"
	"time"
)

const (
	groupOptimisticBeforeMigration = 90000000000018
	groupOptimisticMigration       = 90000000000019
)

// Pre-migration dispatch rows carry arbiter routing decisions. The migration
// must hand them to triage as wake rows without stranding a live lease.
func TestOptimisticMigrationAdoptsExistingDispatchAndResumesLeases(t *testing.T) {
	db, provider := newTestDBAtMigration(t, groupOptimisticBeforeMigration)
	ctx := t.Context()

	const groupID = "11111111-1111-1111-1111-111111111111"
	if _, err := db.Exec(ctx, `INSERT INTO agent (id, name, workspace, sandbox) VALUES ('agent-1', 'Agent One', '/tmp', '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO channel (id, name, type, enabled, config) VALUES ('ch-1', 'Channel One', 'web', true, '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO ctx_group_state (id, platform, platform_group_id, platform_thread_id) VALUES ($1, 'web', 'optimistic', '')`, groupID); err != nil {
		t.Fatal(err)
	}
	for seq, id := range map[int]string{
		1: "a1a1a1a1-0000-0000-0000-000000000001",
		2: "a1a1a1a1-0000-0000-0000-000000000002",
		3: "a1a1a1a1-0000-0000-0000-000000000003",
	} {
		if _, err := db.Exec(ctx, `INSERT INTO ctx_group_message (id, group_id, seq, actor_type, actor_id, content) VALUES ($1, $2, $3, 'human', 'user-1', 'hello')`, id, groupID, seq); err != nil {
			t.Fatal(err)
		}
	}
	liveLease := time.Now().UTC().Add(2 * time.Minute).Truncate(time.Microsecond)
	if _, err := db.Exec(ctx, `
		INSERT INTO ctx_group_dispatch (id, group_message_id, group_id, agent_id, reply_channel_id, status)
		VALUES ('d15a0000-0000-0000-0000-000000000001', 'a1a1a1a1-0000-0000-0000-000000000001', $1, 'agent-1', 'ch-1', 'pending')
	`, groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO ctx_group_dispatch (id, group_message_id, group_id, agent_id, reply_channel_id, status, attempt_count, lease_until)
		VALUES ('d15a0000-0000-0000-0000-000000000002', 'a1a1a1a1-0000-0000-0000-000000000002', $1, 'agent-1', 'ch-1', 'running', 2, $2)
	`, groupID, liveLease); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, groupOptimisticMigration); err != nil {
		t.Fatalf("run optimistic migration: %v", err)
	}

	var pendingKind, runningKind, runningStatus string
	var pendingTriggerSeq, runningTriggerSeq int64
	var resumedLease time.Time
	if err := db.QueryRow(ctx, `SELECT kind, trigger_seq FROM ctx_group_dispatch WHERE id='d15a0000-0000-0000-0000-000000000001'`).Scan(&pendingKind, &pendingTriggerSeq); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT kind, status, lease_until, trigger_seq FROM ctx_group_dispatch WHERE id='d15a0000-0000-0000-0000-000000000002'`).Scan(&runningKind, &runningStatus, &resumedLease, &runningTriggerSeq); err != nil {
		t.Fatal(err)
	}
	if pendingKind != "wake" || runningKind != "wake" {
		t.Fatalf("kinds pending/running=%q/%q, want wake/wake", pendingKind, runningKind)
	}
	if pendingTriggerSeq != 1 || runningTriggerSeq != 2 {
		t.Fatalf("backfilled trigger_seq pending/running=%d/%d, want 1/2", pendingTriggerSeq, runningTriggerSeq)
	}
	if runningStatus != "running" || !resumedLease.Equal(liveLease) {
		t.Fatalf("live lease status/until=%q/%s, want running/%s", runningStatus, resumedLease, liveLease)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO ctx_group_dispatch (id, group_message_id, group_id, agent_id, reply_channel_id, status, trigger_seq)
		VALUES ('d15a0000-0000-0000-0000-000000000003', 'a1a1a1a1-0000-0000-0000-000000000003', $1, 'agent-1', 'ch-1', 'pending', 3)
	`, groupID); err != nil {
		t.Fatal(err)
	}
	var defaultKind string
	if err := db.QueryRow(ctx, `SELECT kind FROM ctx_group_dispatch WHERE id='d15a0000-0000-0000-0000-000000000003'`).Scan(&defaultKind); err != nil {
		t.Fatal(err)
	}
	if defaultKind != "wake" {
		t.Fatalf("post-migration default kind=%q, want wake", defaultKind)
	}
}

// ctx_group_chain_root is the one definition the wake claim gate and the HOLD
// count both read. Pin the three inputs that move it: no history at all, a
// human message at or before the trigger, and this agent's own accepted post.
func TestGroupChainRootTracksHumanAndOwnAcceptedPost(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	const groupID = "22222222-2222-2222-2222-222222222222"
	if _, err := db.Exec(ctx, `INSERT INTO agent (id, name, workspace, sandbox) VALUES ('agent-1', 'Agent One', '/tmp', '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO channel (id, name, type, enabled, config) VALUES ('ch-1', 'Channel One', 'web', true, '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO ctx_group_state (id, platform, platform_group_id, platform_thread_id) VALUES ($1, 'web', 'chain-root', '')`, groupID); err != nil {
		t.Fatal(err)
	}

	chainRoot := func(triggerSeq int64) int64 {
		var root int64
		if err := db.QueryRow(ctx, `SELECT ctx_group_chain_root($1, 'agent-1', $2)`, groupID, triggerSeq).Scan(&root); err != nil {
			t.Fatalf("chain root at trigger %d: %v", triggerSeq, err)
		}
		return root
	}

	if got := chainRoot(10); got != 0 {
		t.Fatalf("empty group chain root = %d, want 0", got)
	}

	const humanID = "b2b2b2b2-0000-0000-0000-000000000003"
	if _, err := db.Exec(ctx, `INSERT INTO ctx_group_message (id, group_id, seq, actor_type, actor_id, content) VALUES ($1, $2, 3, 'human', 'user-1', 'hello')`, humanID, groupID); err != nil {
		t.Fatal(err)
	}
	if got := chainRoot(10); got != 3 {
		t.Fatalf("chain root after human seq 3 = %d, want 3", got)
	}
	// A human message later than the trigger belongs to a chain this wake
	// cannot see, so it must not move the root.
	if got := chainRoot(2); got != 0 {
		t.Fatalf("chain root at trigger 2 = %d, want 0", got)
	}

	// This agent's own accepted post opens a new chain regardless of trigger.
	const ownID = "b2b2b2b2-0000-0000-0000-000000000007"
	if _, err := db.Exec(ctx, `INSERT INTO ctx_group_message (id, group_id, seq, actor_type, actor_id, content) VALUES ($1, $2, 7, 'agent', 'agent-1', 'mine')`, ownID, groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO ctx_group_dispatch (id, group_message_id, group_id, agent_id, reply_channel_id, status, trigger_seq, result_message_id)
		VALUES ('d15a0000-0000-0000-0000-000000000101', $1, $2, 'agent-1', 'ch-1', 'completed', 3, $3)
	`, humanID, groupID, ownID); err != nil {
		t.Fatal(err)
	}
	if got := chainRoot(3); got != 7 {
		t.Fatalf("chain root after own accepted post = %d, want 7", got)
	}

	// The empty-string sentinel on an unaccepted row must not poison the cast.
	if _, err := db.Exec(ctx, `
		INSERT INTO ctx_group_dispatch (id, group_message_id, group_id, agent_id, reply_channel_id, status, trigger_seq)
		VALUES ('d15a0000-0000-0000-0000-000000000102', $1, $2, 'agent-1', 'ch-1', 'pending', 7)
	`, ownID, groupID); err != nil {
		t.Fatal(err)
	}
	if got := chainRoot(7); got != 7 {
		t.Fatalf("chain root with empty result sentinel = %d, want 7", got)
	}
}
