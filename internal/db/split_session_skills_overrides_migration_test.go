package db

import "testing"

const (
	sessionSkillsOverridesBeforeMigration = 90000000000026
	sessionSkillsOverridesMigration       = 90000000000027
)

// The `session` and `skills` unions became six action tools. As in
// 90000000000026, a tool_override row is keyed by name, so a row naming either
// retired union now matches nothing and only waits for a future tool to reuse
// the name and inherit the setting. Stella is pre-production, so the rows go.
//
// The assertion that matters is the second one: deleting by name must not reach
// a name this change did not retire — least of all `skill_load`, which starts
// with the retired union's own prefix.
func TestSplitSessionAndSkillsOverridesDeletesOnlyTheRetiredNames(t *testing.T) {
	db, provider := newTestDBAtMigration(t, sessionSkillsOverridesBeforeMigration)
	ctx := t.Context()
	seedToolOverride(t, db, "session", false)
	seedToolOverride(t, db, "skills", false)
	// Three survivors: a name this change never touched, and the two new names
	// an operator could already have written a row for.
	seedToolOverride(t, db, "memory", false)
	seedToolOverride(t, db, "session_list", true)
	seedToolOverride(t, db, "skill_load", false)

	if _, err := provider.UpTo(ctx, sessionSkillsOverridesMigration); err != nil {
		t.Fatalf("migrate session and skills overrides: %v", err)
	}

	assertExactSystemOverrides(t, db, map[string]bool{"memory": false, "session_list": true, "skill_load": false})
}
