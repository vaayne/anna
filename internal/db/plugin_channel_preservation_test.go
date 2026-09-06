package db

import (
	"testing"

	"github.com/CherryHQ/stella/internal/plugin"
)

func TestPluginImportPreservesIndependentChannelCredentialsAndDisable(t *testing.T) {
	database := newTestDBAtMigrationOnly(t, pluginCutoverMigration41)
	ctx := t.Context()
	preparePluginPolicyCutoverSchema(t, database)
	if _, err := database.Exec(ctx, `INSERT INTO agent(id,name,workspace) VALUES('account-agent-a','A',''),('account-agent-b','B','')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(ctx, `
 INSERT INTO channel(id,name,type,agent_id,enabled,config) VALUES
 ('account-a','Account A','feishu','account-agent-a',true,'{"app_id":"fixture-a","app_secret":"fixture-secret-a"}'),
 ('account-b','Account B','feishu','account-agent-b',false,'{"app_id":"fixture-b","app_secret":"fixture-secret-b"}')`); err != nil {
		t.Fatal(err)
	}
	// Conflicting old writers must retain the restrictive decision, regardless
	// of which account was last mirrored into the old global plugin row.
	if _, err := database.Exec(ctx, `INSERT INTO plugin(id,kind,name,enabled,config) VALUES('channel/feishu','channel','feishu',true,'{"app_id":"fixture-b","app_secret":"fixture-secret-b"}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(ctx, `INSERT INTO plugin_override(plugin_id,enabled) VALUES('channel/feishu',false)`); err != nil {
		t.Fatal(err)
	}
	snapshotChannels := func() string {
		t.Helper()
		var snapshot string
		if err := database.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(c) ORDER BY c.id)::text FROM channel c`).Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	before := snapshotChannels()
	catalog := plugin.NewCatalog()
	definition := plugin.Definition{ID: "channel/feishu", Namespace: "feishu", DisplayName: "Feishu", Backend: plugin.BackendGo, Source: plugin.SourceBuiltin, ImplementationKey: "channel/feishu", Spec: []byte(`{}`), DefaultEnabled: true, Revision: 1}
	if err := catalog.Register(definition); err != nil {
		t.Fatal(err)
	}
	if err := plugin.ImportLegacyState(ctx, database, catalog, nil); err != nil {
		t.Fatal(err)
	}
	if after := snapshotChannels(); after != before {
		t.Fatal("import changed channel identities, credentials, bindings, or active states")
	}
	service := plugin.NewService(database, nil, catalog, plugin.BackendPolicy{Validate: noopPluginValidator, Transition: noopBackendTransition}, inlinePluginMutationFence)
	if err := service.SyncBuiltinDefaults(ctx); err != nil {
		t.Fatal(err)
	}
	for _, agentID := range []string{"account-agent-a", "account-agent-b"} {
		enabled, err := service.AdministrativeCap(ctx, definition.ID, agentID)
		if err != nil || enabled {
			t.Fatalf("imported admin disable lost for %s: enabled=%v err=%v", agentID, enabled, err)
		}
	}
	var configs int
	if err := database.QueryRow(ctx, `SELECT count(*) FROM plugin_config WHERE plugin_id=$1 AND scope='system' AND enabled=false AND config='{}'::jsonb AND credential_refs='{}'::jsonb`, definition.ID).Scan(&configs); err != nil {
		t.Fatal(err)
	}
	if configs != 1 {
		t.Fatalf("expected one disabled platform config without channel credentials, got %d", configs)
	}
}
