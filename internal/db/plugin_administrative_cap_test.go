package db

import (
	"testing"

	"github.com/CherryHQ/stella/internal/plugin"
)

func TestUnifiedPluginAdministrativeCap(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		shipped, system, agent bool
		target                 string
		want                   bool
	}{
		{"system ceiling", true, false, true, "agent-a", false},
		{"matching agent ceiling", true, true, false, "agent-a", false},
		{"other agent ceiling", true, true, false, "agent-b", true},
		{"explicit agent enables default-off", false, true, true, "agent-a", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			ctx := t.Context()
			def := pluginDefinition("channel/test", "test", tc.shipped)
			catalog := plugin.NewCatalog()
			if err := catalog.Register(def); err != nil {
				t.Fatal(err)
			}
			svc := plugin.NewService(db, nil, catalog, plugin.BackendPolicy{Validate: noopPluginValidator, Transition: noopBackendTransition}, inlinePluginMutationFence)
			if err := svc.SyncBuiltinDefaults(ctx); err != nil {
				t.Fatal(err)
			}
			var system any = tc.system
			if !tc.shipped {
				system = nil
			}
			if _, err := db.Exec(ctx, `UPDATE plugin_config SET enabled=$1 WHERE plugin_id=$2 AND scope='system'`, system, def.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(ctx, `INSERT INTO agent(id,name,workspace) VALUES('agent-a','A',''),('agent-b','B','')`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(ctx, `INSERT INTO plugin_config(plugin_id,namespace,scope,agent_id,enabled,config) VALUES($1,$2,'system_agent','agent-a',$3,'{}')`, def.ID, def.Namespace, tc.agent); err != nil {
				t.Fatal(err)
			}
			got, err := svc.AdministrativeCap(ctx, def.ID, tc.target)
			if err != nil || got != tc.want {
				t.Fatalf("cap = %v, %v; want %v", got, err, tc.want)
			}
			for _, id := range []string{"custom/missing", "channel/missing"} {
				if got, err := svc.AdministrativeCap(ctx, id, tc.target); err == nil || got {
					t.Fatalf("unknown %s permitted: %v %v", id, got, err)
				}
			}
			dormant := plugin.NewService(db, nil, plugin.NewCatalog(), plugin.BackendPolicy{Validate: noopPluginValidator, Transition: noopBackendTransition}, inlinePluginMutationFence)
			if got, err := dormant.AdministrativeCap(ctx, def.ID, tc.target); err == nil || got {
				t.Fatalf("dormant definition permitted: %v %v", got, err)
			}
		})
	}
}
