package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	storepkg "github.com/CherryHQ/stella/cmd/stellad/store"
	"github.com/CherryHQ/stella/internal/agent"
	"github.com/CherryHQ/stella/internal/auth"
	"github.com/CherryHQ/stella/internal/authz"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	appdb "github.com/CherryHQ/stella/internal/db"
	"github.com/CherryHQ/stella/internal/db/dbtest"
	"github.com/CherryHQ/stella/internal/mcp"
	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/internal/vault"
	"github.com/CherryHQ/stella/pkg/db/pgnull"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

const integrationPluginID = "custom/mcp-integration"

// setup builds an MCP service backed by a real database and vault, plus a
// provisioned user and agent so all four scopes are usable.
func setup(t *testing.T) (svc *mcp.Service, q *sqlc.Queries, userID, agentID string) {
	t.Helper()
	db := dbtest.New(t)
	oidc := appdb.NewOIDCStore(db)
	q = sqlc.New(db)
	ctx := context.Background()

	masterID, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("master identity: %v", err)
	}
	vaultSvc, err := vault.NewServiceForPool(db, masterID.String(), nil)
	if err != nil {
		t.Fatalf("vault.NewService: %v", err)
	}

	user, err := oidc.CreateUser(ctx, auth.User{ID: uuid.NewString(), Email: "u@mcp.test", Name: "U"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	pub, encPriv, err := vault.GenerateUserKeys(vaultSvc.MasterRecipient())
	if err != nil {
		t.Fatalf("GenerateUserKeys: %v", err)
	}
	if err := oidc.UpdateUserAgeKeys(ctx, user.ID, pub, encPriv); err != nil {
		t.Fatalf("UpdateUserAgeKeys: %v", err)
	}

	agent, err := q.CreateAgent(ctx, sqlc.CreateAgentParams{
		ID: "mcp-test-agent", Name: "Agent", Workspace: "/tmp/agent",
		Sandbox: json.RawMessage(`{}`),
		Scope:   "system", Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	svc = mcp.NewServiceForPool(db, vaultSvc, func(tx pgx.Tx) mcp.Vault { return vaultSvc.WithTx(tx) })
	policy := mcp.EndpointPolicy{AllowPrivate: true}
	svc.SetEndpointPolicy(policy)
	agents := agentaccess.NewService(storepkg.NewDBStore(db), appdb.NewAuthStore(db))
	svc.SetPluginService(plugin.NewService(db, agents, plugin.NewCatalog(), mcp.NewMCPBackendPolicy(policy), func(_ context.Context, fn func() error) error {
		return fn()
	}))
	if _, err := db.Exec(ctx, `INSERT INTO plugin_definition(id,namespace,display_name,backend,source,implementation_key,spec,default_enabled,revision,creator_user_id) VALUES($1,'mcp_integration','MCP integration','mcp','custom','mcp','{}',false,1,$2)`, integrationPluginID, user.ID); err != nil {
		t.Fatalf("Create common MCP definition: %v", err)
	}
	if _, err := db.Exec(ctx, `
		ALTER TABLE tool_override ALTER COLUMN tool_name DROP NOT NULL;
		ALTER TABLE tool_override DROP CONSTRAINT IF EXISTS tool_override_tool_name_scope_user_id_agent_id_key;
		CREATE UNIQUE INDEX IF NOT EXISTS test_mcp_tool_override_plugin_identity
			ON tool_override (plugin_id, local_tool_name, scope, user_id, agent_id) NULLS NOT DISTINCT
			WHERE tool_name IS NULL;
	`); err != nil {
		t.Fatalf("prepare plugin tool override identity schema: %v", err)
	}
	return svc, q, user.ID, agent.ID
}

func integrationContext(t *testing.T, userID string, admin bool) context.Context {
	t.Helper()
	authority, err := authz.NewUserAuthority(authz.UserID(userID), admin)
	if err != nil {
		t.Fatalf("new authority: %v", err)
	}
	return authz.WithAuthority(context.Background(), authority)
}

// TestCredentialEncryptedAtRest proves the bearer token is stored age-encrypted
// in vault_entry (unreadable at rest) and never lands in the mcp_server row.
func TestCredentialEncryptedAtRest(t *testing.T) {
	svc, q, userID, _ := setup(t)
	ctx := integrationContext(t, userID, true)
	const token = "ghp_super_secret_value_1234567890"

	reg, err := svc.Create(ctx, mcp.CreateInput{
		PluginID: integrationPluginID, Scope: mcp.ScopeUser, UserID: userID, Name: "gh", URL: "https://mcp.example.com",
		Transport: mcp.TransportStreamableHTTP, AuthType: mcp.AuthTypeBearer, Token: token,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The common config must carry only a locator, no secret.
	configs, err := q.ListPluginConfigs(ctx, integrationPluginID)
	if err != nil || len(configs) != 1 {
		t.Fatalf("read common config: %v (rows=%d)", err, len(configs))
	}
	if strings.Contains(string(configs[0].Config), token) || strings.Contains(string(configs[0].CredentialRefs), token) {
		t.Fatal("common config or refs contain the bearer token")
	}

	// The vault ciphertext must not reveal the token.
	entry, err := q.GetVaultEntryByScope(ctx, sqlc.GetVaultEntryByScopeParams{
		Scope: mcp.ScopeUser, UserID: pgnull.Text(userID), Name: reg.CredentialRef,
	})
	if err != nil {
		t.Fatalf("GetVaultEntryByScope: %v", err)
	}
	if strings.Contains(entry.Ciphertext, token) {
		t.Fatal("ciphertext leaks the plaintext token")
	}
	if !strings.Contains(entry.Ciphertext, "BEGIN AGE ENCRYPTED FILE") {
		t.Fatalf("ciphertext is not age-armored: %q", entry.Ciphertext)
	}

	// The service still decrypts it back for connecting.
	got, err := svc.BearerToken(ctx, reg)
	if err != nil {
		t.Fatalf("BearerToken: %v", err)
	}
	if got != token {
		t.Fatalf("BearerToken = %q, want %q", got, token)
	}
}

// TestCommonConfigValidatorRedactsMalformedEndpoint verifies that common
// config validation reports only a safe policy error for malformed input.
func TestUpdateRedactsMalformedLegacyEndpoint(t *testing.T) {
	ctx := context.Background()
	const raw = "https://legacy-user:legacy-pass@example.test/%zz?token=legacy-query#legacy-fragment"
	def := plugin.Definition{ID: integrationPluginID, Namespace: "mcp_integration", DisplayName: "MCP integration", Backend: plugin.BackendMCP, Source: plugin.SourceCustom, ImplementationKey: "mcp", Spec: json.RawMessage(`{}`), Revision: 1}
	enabled := true
	cfg := plugin.Config{ID: uuid.NewString(), PluginID: def.ID, Namespace: def.Namespace, Scope: plugin.ScopeUser, UserID: "user", Enabled: &enabled, Payload: json.RawMessage(`{"url":"` + raw + `","transport":"streamable_http","auth_type":"none","credential_mode":"shared"}`), CredentialRefs: json.RawMessage(`{}`), Revision: 1}
	err := mcp.ValidateMCPPayload(ctx, mcp.EndpointPolicy{AllowPrivate: true}, def, cfg, nil)
	if err == nil {
		t.Fatal("malformed common URL succeeded validation")
	}
	got := err.Error()
	for _, secret := range []string{"legacy-user", "legacy-pass", "legacy-query", "legacy-fragment", raw} {
		if strings.Contains(got, secret) {
			t.Fatalf("update error leaked %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "endpoint") {
		t.Fatalf("validation error lost safe diagnosis: %s", got)
	}
}

func namePtr(s string) *string { return &s }

// TestRenamePreservesPluginToolOverrides proves policy identity remains the
// stable plugin/local pair when a config's display name changes.
func TestRenamePreservesPluginToolOverrides(t *testing.T) {
	svc, q, userID, agentID := setup(t)
	ctx := integrationContext(t, userID, true)

	reg, err := svc.Create(ctx, mcp.CreateInput{
		PluginID: integrationPluginID, Scope: mcp.ScopeUser, UserID: userID, Name: "fo", URL: "https://mcp.example.com",
		Transport: mcp.TransportStreamableHTTP, AuthType: mcp.AuthTypeNone,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	const localTool = "list"
	seed := []sqlc.UpsertPluginToolOverrideParams{
		{PluginID: pgnull.Text(integrationPluginID), LocalToolName: pgnull.Text(localTool), Scope: agent.ToolOverrideScopeSystem, Enabled: true},
		{PluginID: pgnull.Text(integrationPluginID), LocalToolName: pgnull.Text(localTool), Scope: agent.ToolOverrideScopeUser, UserID: pgnull.Text(userID), Enabled: false},
	}
	for _, arg := range seed {
		if _, err := q.UpsertPluginToolOverride(ctx, arg); err != nil {
			t.Fatalf("seed override %+v: %v", arg, err)
		}
	}

	updated, err := svc.Update(ctx, mcp.UpdateInput{ID: reg.ID, Scope: mcp.ScopeUser, UserID: userID, Name: namePtr("fo2")})
	if err != nil {
		t.Fatalf("rename Update: %v", err)
	}
	if updated.Name != "fo2" {
		t.Fatalf("name = %q", updated.Name)
	}

	rows, err := q.ListToolOverridesForAgentContext(ctx, sqlc.ListToolOverridesForAgentContextParams{
		UserID: pgnull.Text(userID), AgentID: pgnull.Text(agentID),
	})
	if err != nil {
		t.Fatalf("list overrides: %v", err)
	}
	got := 0
	for _, row := range rows {
		if row.PluginID.Valid && row.PluginID.String == integrationPluginID && row.LocalToolName.Valid && row.LocalToolName.String == localTool {
			got++
		}
	}
	if got != 2 {
		t.Fatalf("plugin override rows after rename = %d, want 2, rows=%+v", got, rows)
	}
}

// TestDeletePreservesPluginToolOverrides proves deleting one config does not
// remove policy rows owned by the plugin or by another config.
func TestDeletePreservesPluginToolOverrides(t *testing.T) {
	svc, q, userID, agentID := setup(t)
	ctx := integrationContext(t, userID, true)

	reg, err := svc.Create(ctx, mcp.CreateInput{
		PluginID: integrationPluginID, Scope: mcp.ScopeSystem, Name: "gh", URL: "https://mcp.example.com",
		Transport: mcp.TransportStreamableHTTP, AuthType: mcp.AuthTypeNone,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Create(ctx, mcp.CreateInput{
		PluginID: integrationPluginID, Scope: mcp.ScopeUser, UserID: userID, Name: "gh", URL: "https://mcp.example.com/other",
		Transport: mcp.TransportStreamableHTTP, AuthType: mcp.AuthTypeNone,
	}); err != nil {
		t.Fatalf("Create other config: %v", err)
	}
	const localTool = "list"
	for _, arg := range []sqlc.UpsertPluginToolOverrideParams{
		{PluginID: pgnull.Text(integrationPluginID), LocalToolName: pgnull.Text(localTool), Scope: agent.ToolOverrideScopeSystem, Enabled: true},
		{PluginID: pgnull.Text(integrationPluginID), LocalToolName: pgnull.Text(localTool), Scope: agent.ToolOverrideScopeUser, UserID: pgnull.Text(userID), Enabled: true},
	} {
		if _, err := q.UpsertPluginToolOverride(ctx, arg); err != nil {
			t.Fatalf("seed override %+v: %v", arg, err)
		}
	}

	if err := svc.Delete(ctx, reg.ID, mcp.ScopeSystem, "", ""); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	rows, err := q.ListToolOverridesForAgentContext(ctx, sqlc.ListToolOverridesForAgentContextParams{
		UserID: pgnull.Text(userID), AgentID: pgnull.Text(agentID),
	})
	if err != nil {
		t.Fatalf("list overrides: %v", err)
	}
	pluginRows := 0
	for _, row := range rows {
		if row.PluginID.Valid && row.PluginID.String == integrationPluginID && row.LocalToolName.Valid && row.LocalToolName.String == localTool {
			pluginRows++
		}
	}
	if pluginRows != 2 {
		t.Fatalf("plugin override rows after config delete = %d, want 2, rows=%+v", pluginRows, rows)
	}
}
