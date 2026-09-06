package db

import "testing"

const (
	dropRTKPluginBeforeMigration = 90000000000016
	dropRTKPluginMigration       = 90000000000017
)

func TestDropRTKPlugin(t *testing.T) {
	db, provider := newTestDBAtMigration(t, dropRTKPluginBeforeMigration)
	ctx := t.Context()
	for _, id := range []string{"hook/rtk", "tool/rg"} {
		seedPluginRow(t, db, id)
		if _, err := db.Exec(ctx, `
			INSERT INTO plugin_override (plugin_id, enabled, config)
			VALUES ($1, true, '{}'::jsonb)
		`, id); err != nil {
			t.Fatalf("seed plugin override %q: %v", id, err)
		}
	}

	if _, err := provider.UpTo(ctx, dropRTKPluginMigration); err != nil {
		t.Fatalf("run RTK cleanup: %v", err)
	}
	for _, probe := range []struct {
		name  string
		query string
		want  bool
	}{
		{"RTK plugin", `SELECT EXISTS (SELECT 1 FROM plugin WHERE id = 'hook/rtk')`, false},
		{"RTK state", `SELECT EXISTS (SELECT 1 FROM plugin_state WHERE plugin_id = 'hook/rtk')`, false},
		{"RTK override", `SELECT EXISTS (SELECT 1 FROM plugin_override WHERE plugin_id = 'hook/rtk')`, false},
		{"unrelated plugin", `SELECT EXISTS (SELECT 1 FROM plugin WHERE id = 'tool/rg')`, true},
		{"unrelated state", `SELECT EXISTS (SELECT 1 FROM plugin_state WHERE plugin_id = 'tool/rg')`, true},
		{"unrelated override", `SELECT EXISTS (SELECT 1 FROM plugin_override WHERE plugin_id = 'tool/rg')`, true},
	} {
		var got bool
		if err := db.QueryRow(ctx, probe.query).Scan(&got); err != nil {
			t.Fatalf("query %s: %v", probe.name, err)
		}
		if got != probe.want {
			t.Errorf("%s exists = %v, want %v", probe.name, got, probe.want)
		}
	}
}
