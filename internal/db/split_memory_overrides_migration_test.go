package db

import "testing"

const (
	memoryOverridesBeforeMigration = 90000000000027
	memoryOverridesMigration       = 90000000000028
)

// The `memory` union became two action tools. As in 90000000000026 and
// 90000000000027, a tool_override row is keyed by name, so a row naming the
// retired union now matches nothing and only waits for a future tool to reuse
// the name and inherit the setting. Stella is pre-production, so the row goes.
//
// The assertion that matters is the second one: deleting by name must not reach
// a name this change did not retire — least of all `memory_search`, which
// starts with the retired union's own name.
func TestSplitMemoryOverridesDeletesOnlyTheRetiredName(t *testing.T) {
	db, provider := newTestDBAtMigration(t, memoryOverridesBeforeMigration)
	ctx := t.Context()
	seedToolOverride(t, db, "memory", false)
	// Three survivors: a name this change never touched, and the two new names
	// an operator could already have written a row for.
	seedToolOverride(t, db, "session_list", true)
	seedToolOverride(t, db, "memory_search", false)
	seedToolOverride(t, db, "memory_read", true)

	if _, err := provider.UpTo(ctx, memoryOverridesMigration); err != nil {
		t.Fatalf("migrate memory overrides: %v", err)
	}

	assertExactSystemOverrides(t, db, map[string]bool{"session_list": true, "memory_search": false, "memory_read": true})
}
