package vault_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	storepkg "github.com/CherryHQ/stella/cmd/stellad/store"
	"github.com/CherryHQ/stella/internal/auth"
	"github.com/CherryHQ/stella/internal/authz"
	oauth "github.com/CherryHQ/stella/internal/connections/oauth"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	appdb "github.com/CherryHQ/stella/internal/db"
	"github.com/CherryHQ/stella/internal/db/dbtest"
	"github.com/CherryHQ/stella/internal/plugin/manifest"
	"github.com/CherryHQ/stella/internal/vault"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

// vaultTestDB combines OIDCStore (for auth_user) with sqlc.Queries (for vault_entry).
type vaultTestDB struct {
	oidc *appdb.OIDCStore
	q    *sqlc.Queries
}

func (d *vaultTestDB) GetVaultUser(ctx context.Context, id string) (sqlc.VaultUser, error) {
	u, err := d.oidc.GetUser(ctx, id)
	if err != nil {
		return sqlc.VaultUser{}, err
	}
	return sqlc.VaultUser{AgePublicKey: u.AgePublicKey, AgePrivateKey: u.AgePrivateKey}, nil
}

func (d *vaultTestDB) GetVaultEntryByScope(ctx context.Context, arg sqlc.GetVaultEntryByScopeParams) (sqlc.VaultEntry, error) {
	return d.q.GetVaultEntryByScope(ctx, arg)
}

func (d *vaultTestDB) ListVaultEntriesByScope(ctx context.Context, arg sqlc.ListVaultEntriesByScopeParams) ([]sqlc.VaultEntry, error) {
	return d.q.ListVaultEntriesByScope(ctx, arg)
}

func (d *vaultTestDB) ListVaultEntriesForRuntime(ctx context.Context, arg sqlc.ListVaultEntriesForRuntimeParams) ([]sqlc.VaultEntry, error) {
	return d.q.ListVaultEntriesForRuntime(ctx, arg)
}

func (d *vaultTestDB) UpsertVaultEntryByScope(ctx context.Context, arg sqlc.UpsertVaultEntryByScopeParams) (sqlc.VaultEntry, error) {
	return d.q.UpsertVaultEntryByScope(ctx, arg)
}

func (d *vaultTestDB) DeleteVaultEntryByScope(ctx context.Context, arg sqlc.DeleteVaultEntryByScopeParams) error {
	return d.q.DeleteVaultEntryByScope(ctx, arg)
}

// testService sets up a vault Service backed by a real SQLite database. It
// creates a user with age keys provisioned and returns the service, oidcStore,
// and the created user ID.
func testService(t *testing.T) (*vault.Service, *appdb.OIDCStore, string) {
	t.Helper()
	svc, oidc, userID, _ := testServiceWithQueries(t)
	return svc, oidc, userID
}

func testServiceWithQueries(t *testing.T) (*vault.Service, *appdb.OIDCStore, string, *sqlc.Queries) {
	t.Helper()

	db := dbtest.New(t)

	oidc := appdb.NewOIDCStore(db)
	q := sqlc.New(db)
	ctx := context.Background()

	masterID, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity (master): %v", err)
	}

	testDB := &vaultTestDB{oidc: oidc, q: q}
	agents := agentaccess.NewService(storepkg.NewDBStore(db), appdb.NewAuthStore(db))
	svc, err := vault.NewService(testDB, masterID.String(), agents)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	user, err := oidc.CreateUser(ctx, auth.User{
		ID:    uuid.NewString(),
		Email: "testuser@vault.test",
		Name:  "Test User",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	pubKey, encPrivKey, err := vault.GenerateUserKeys(svc.MasterRecipient())
	if err != nil {
		t.Fatalf("GenerateUserKeys: %v", err)
	}
	if err := oidc.UpdateUserAgeKeys(ctx, user.ID, pubKey, encPrivKey); err != nil {
		t.Fatalf("UpdateUserAgeKeys: %v", err)
	}

	return svc, oidc, user.ID, q
}

func agentAuthority(t *testing.T, userID, agentID string) authz.Authority {
	t.Helper()
	a, err := agentaccess.WorkerAgentAuthority(userID, agentID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestVaultAccessEnforcesAgentScope(t *testing.T) {
	svc, _, userID, q := testServiceWithQueries(t)
	ctx := context.Background()
	for _, agentID := range []string{"agent-a", "agent-b"} {
		if _, err := q.CreateAgent(ctx, sqlc.CreateAgentParams{ID: agentID, Name: agentID, Model: "test/model", Workspace: "workspace", Sandbox: json.RawMessage("{}"), Scope: "system", Enabled: true}); err != nil {
			t.Fatalf("CreateAgent(%s): %v", agentID, err)
		}
	}
	begin := func(authority authz.Authority) *vault.Access {
		acc, err := svc.Begin(ctx, authority)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		return acc
	}
	authA := agentAuthority(t, userID, "agent-a")
	authB := agentAuthority(t, userID, "agent-b")

	// An invalid Authority is refused at Begin.
	if _, err := svc.Begin(ctx, authz.Authority{}); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("Begin(zero) err=%v, want forbidden", err)
	}
	// A delegated agent (non-admin) cannot touch admin-managed system scopes.
	for _, scope := range []string{vault.ScopeSystem, vault.ScopeSystemAgent} {
		if _, err := begin(authA).ListScoped(ctx, scope, "agent-a"); !errors.Is(err, authz.ErrForbidden) {
			t.Fatalf("List(%s) err=%v, want forbidden", scope, err)
		}
		if err := begin(authA).SetScoped(ctx, scope, "agent-a", "SECRET", "x", vault.SetOptions{}); !errors.Is(err, authz.ErrForbidden) {
			t.Fatalf("Set(%s) err=%v, want forbidden", scope, err)
		}
	}

	// agent-a writes its own user_agent bucket.
	if err := begin(authA).SetScoped(ctx, vault.ScopeUserAgent, "agent-a", "AGENT_SECRET", "a", vault.SetOptions{}); err != nil {
		t.Fatalf("Set user_agent: %v", err)
	}
	// agent-b cannot even name agent-a's bucket (confinement).
	if _, err := begin(authB).ListScoped(ctx, vault.ScopeUserAgent, "agent-a"); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("agent-b listing agent-a bucket err=%v, want forbidden", err)
	}
	// agent-b's own bucket is empty and deleting AGENT_SECRET there is a no-op.
	entries, err := begin(authB).ListScoped(ctx, vault.ScopeUserAgent, "")
	if err != nil {
		t.Fatalf("List own agent: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("agent-b saw entries: %+v", entries)
	}
	if err := begin(authB).DeleteScoped(ctx, vault.ScopeUserAgent, "", "AGENT_SECRET"); err != nil {
		t.Fatalf("Delete own bucket: %v", err)
	}
	// agent-a still owns AGENT_SECRET.
	entries, err = begin(authA).ListScoped(ctx, vault.ScopeUserAgent, "")
	if err != nil {
		t.Fatalf("List owner agent: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "AGENT_SECRET" {
		t.Fatalf("owner agent entry missing: %+v", entries)
	}
}

func TestSetAndList(t *testing.T) {
	t.Parallel()
	svc, _, userID := testService(t)
	ctx := context.Background()

	if err := svc.Set(ctx, userID, "GITHUB_TOKEN", "ghp_secret"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	entries, err := svc.List(ctx, userID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("List: got %d entries, want 1", len(entries))
	}
	if entries[0].Name != "GITHUB_TOKEN" {
		t.Errorf("Name = %q, want %q", entries[0].Name, "GITHUB_TOKEN")
	}
	if entries[0].CreatedAt == "" {
		t.Error("CreatedAt is empty")
	}
	if entries[0].UpdatedAt == "" {
		t.Error("UpdatedAt is empty")
	}
}

func TestSetScopedRejectsSystemScope(t *testing.T) {
	t.Parallel()
	svc, _, _ := testService(t)
	ctx := context.Background()

	if err := svc.SetScoped(ctx, vault.ScopeSystem, "", "", "GLOBAL_TOKEN", "value"); err == nil {
		t.Fatal("SetScoped should reject system scope")
	}
	if err := svc.SetSystemScoped(ctx, vault.ScopeSystem, "", "GLOBAL_TOKEN", "value"); err != nil {
		t.Fatalf("SetSystemScoped: %v", err)
	}
	entries, err := svc.ListSystemScoped(ctx, vault.ScopeSystem, "")
	if err != nil {
		t.Fatalf("ListSystemScoped: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "GLOBAL_TOKEN" {
		t.Fatalf("entries = %+v, want GLOBAL_TOKEN", entries)
	}
}

func TestSetValidation(t *testing.T) {
	t.Parallel()
	svc, _, userID := testService(t)
	ctx := context.Background()

	invalid := []string{
		"",
		"lowercase",
		"123START",
		"HAS SPACE",
		"STELLA_SECRET",
		"STELLA_TOKEN",
		"PATH",
		"HOME",
		"LC_ALL",
	}
	for _, name := range invalid {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := svc.Set(ctx, userID, name, "value"); err == nil {
				t.Errorf("Set(%q) = nil, want error", name)
			}
		})
	}
}

func TestLoadEnvDefaultSecretIsAmbient(t *testing.T) {
	t.Parallel()
	svc, _, userID := testService(t)
	ctx := context.Background()

	if err := svc.Set(ctx, userID, "API_KEY", "sk_test_123"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	env, err := svc.LoadEnvForAgent(ctx, userID, "")
	if err != nil {
		t.Fatalf("LoadEnvForAgent: %v", err)
	}
	if got := env["API_KEY"]; got != "sk_test_123" {
		t.Fatalf("API_KEY = %q, want sk_test_123", got)
	}
	got, ok, err := svc.Lookup(ctx, userID, "API_KEY")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !ok {
		t.Fatal("Lookup ok = false, want true")
	}
	if got != "sk_test_123" {
		t.Fatalf("Lookup API_KEY = %q, want sk_test_123", got)
	}
}

func TestLookupAbsentReturnsNotFound(t *testing.T) {
	t.Parallel()
	svc, _, userID := testService(t)
	ctx := context.Background()

	got, ok, err := svc.Lookup(ctx, userID, "API_KEY")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if ok {
		t.Fatal("Lookup ok = true, want false")
	}
	if got != "" {
		t.Fatalf("Lookup value = %q, want empty", got)
	}
}

func TestLoadEnvForAgentMergesScopedPrecedence(t *testing.T) {
	t.Parallel()
	svc, _, userID, q := testServiceWithQueries(t)
	ctx := context.Background()

	if _, err := q.CreateAgent(ctx, sqlc.CreateAgentParams{
		ID: "agent-a", Name: "Agent A", Model: "test/model", Workspace: "workspace", Sandbox: json.RawMessage("{}"), Scope: "system", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if _, err := q.CreateAgent(ctx, sqlc.CreateAgentParams{
		ID: "agent-b", Name: "Agent B", Model: "test/model", Workspace: "workspace", Sandbox: json.RawMessage("{}"), Scope: "system", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	sets := []struct {
		scope   string
		agentID string
		value   string
	}{
		{scope: vault.ScopeSystem, value: "system"},
		{scope: vault.ScopeSystemAgent, agentID: "agent-a", value: "system-agent"},
		{scope: vault.ScopeUser, value: "user"},
		{scope: vault.ScopeUserAgent, agentID: "agent-a", value: "user-agent"},
	}
	for _, set := range sets {
		var err error
		if set.scope == vault.ScopeSystem || set.scope == vault.ScopeSystemAgent {
			err = svc.SetSystemScoped(ctx, set.scope, set.agentID, "TOKEN", set.value)
		} else {
			err = svc.SetScoped(ctx, set.scope, userIDForScope(set.scope, userID), set.agentID, "TOKEN", set.value)
		}
		if err != nil {
			t.Fatalf("set %s: %v", set.scope, err)
		}
	}

	env, err := svc.LoadEnvForAgent(ctx, userID, "agent-a")
	if err != nil {
		t.Fatalf("LoadEnvForAgent(agent-a): %v", err)
	}
	if got := env["TOKEN"]; got != "user-agent" {
		t.Fatalf("TOKEN for agent-a = %q, want user-agent", got)
	}

	env, err = svc.LoadEnvForAgent(ctx, userID, "agent-b")
	if err != nil {
		t.Fatalf("LoadEnvForAgent(agent-b): %v", err)
	}
	if got := env["TOKEN"]; got != "user" {
		t.Fatalf("TOKEN for agent-b = %q, want user", got)
	}
}

func TestListAmbientSecretMetasUsesMetadataOnlyPrecedence(t *testing.T) {
	t.Parallel()
	svc, _, userID, q := testServiceWithQueries(t)
	ctx := context.Background()

	if _, err := q.CreateAgent(ctx, sqlc.CreateAgentParams{
		ID: "agent-a", Name: "Agent A", Model: "test/model", Workspace: "workspace", Sandbox: json.RawMessage("{}"), Scope: "system", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	desc := func(s string) *string { return &s }
	if err := svc.SetSystemScopedWithOptions(ctx, vault.ScopeSystem, "", "TOKEN", "system", vault.SetOptions{Description: desc("system token")}); err != nil {
		t.Fatalf("SetSystemScopedWithOptions: %v", err)
	}
	if err := svc.SetScopedWithOptions(ctx, vault.ScopeUser, userID, "", "TOKEN", "user", vault.SetOptions{Description: desc("user token")}); err != nil {
		t.Fatalf("SetScopedWithOptions TOKEN: %v", err)
	}
	if err := svc.SetScopedWithOptions(ctx, vault.ScopeUserAgent, userID, "agent-a", "API_KEY", "api", vault.SetOptions{Description: desc("API key")}); err != nil {
		t.Fatalf("SetScopedWithOptions API_KEY: %v", err)
	}
	if err := svc.SetScoped(ctx, vault.ScopeUser, userID, "", oauth.VaultKeyGitHub, "oauth-bundle"); err != nil {
		t.Fatalf("SetScoped oauth: %v", err)
	}

	metas, err := svc.ListAmbientSecretMetas(ctx, userID, "agent-a")
	if err != nil {
		t.Fatalf("ListAmbientSecretMetas: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("metas = %#v, want 2 ambient entries", metas)
	}
	if metas[0].Name != "API_KEY" || metas[0].Description != "API key" {
		t.Fatalf("metas[0] = %#v, want API_KEY metadata", metas[0])
	}
	if metas[1].Name != "TOKEN" || metas[1].Description != "user token" {
		t.Fatalf("metas[1] = %#v, want user TOKEN metadata to override system", metas[1])
	}
}

func userIDForScope(scope string, userID string) string {
	if scope == vault.ScopeUser || scope == vault.ScopeUserAgent {
		return userID
	}
	return ""
}

func TestNewServiceInvalidKey(t *testing.T) {
	t.Parallel()
	db := dbtest.New(t)

	oidc := appdb.NewOIDCStore(db)
	testDB := &vaultTestDB{oidc: oidc, q: sqlc.New(db)}
	_, err := vault.NewService(testDB, "not-a-valid-age-key", nil)
	if err == nil {
		t.Fatal("NewService with invalid key should fail")
	}
}

func TestSetNoAgeKeys(t *testing.T) {
	t.Parallel()
	db := dbtest.New(t)

	oidc := appdb.NewOIDCStore(db)
	q := sqlc.New(db)
	ctx := context.Background()

	masterID, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}

	testDB := &vaultTestDB{oidc: oidc, q: q}
	svc, err := vault.NewService(testDB, masterID.String(), nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	user, err := oidc.CreateUser(ctx, auth.User{
		ID:    uuid.NewString(),
		Email: "nokeys@vault.test",
		Name:  "No Keys",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := svc.Set(ctx, user.ID, "MY_KEY", "value"); err == nil {
		t.Fatal("Set should fail for user without age keys")
	}
}

func TestLoadEnvForAgentKeepsSystemSecretsWhenUserEntryFails(t *testing.T) {
	t.Parallel()
	svc, _, userID, q := testServiceWithQueries(t)
	ctx := context.Background()

	if err := svc.SetSystemScoped(ctx, vault.ScopeSystem, "", "GLOBAL_TOKEN", "system-value"); err != nil {
		t.Fatalf("SetSystemScopedWithOptions: %v", err)
	}
	if _, err := q.UpsertVaultEntryByScope(ctx, sqlc.UpsertVaultEntryByScopeParams{
		ID: uuid.NewString(), Scope: vault.ScopeUser, UserID: sqlcNullString(userID), Name: "BROKEN_TOKEN", Ciphertext: "not-age",
	}); err != nil {
		t.Fatalf("insert broken user entry: %v", err)
	}

	env, err := svc.LoadEnvForAgent(ctx, userID, "agent-a")
	if err != nil {
		t.Fatalf("LoadEnvForAgent: %v", err)
	}
	if got := env["GLOBAL_TOKEN"]; got != "system-value" {
		t.Fatalf("GLOBAL_TOKEN = %q, want system-value", got)
	}
	if _, ok := env["BROKEN_TOKEN"]; ok {
		t.Fatal("BROKEN_TOKEN should be skipped")
	}
}

func sqlcNullString(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: value != ""}
}

func insertUserVaultEntry(t *testing.T, q *sqlc.Queries, userID string, publicKey string, name string, value string) {
	t.Helper()
	ciphertext, err := vault.Encrypt(publicKey, value)
	if err != nil {
		t.Fatalf("Encrypt %s: %v", name, err)
	}
	if _, err := q.UpsertVaultEntryByScope(context.Background(), sqlc.UpsertVaultEntryByScopeParams{ID: uuid.NewString(), Scope: vault.ScopeUser, UserID: sqlcNullString(userID), Name: name, Ciphertext: ciphertext}); err != nil {
		t.Fatalf("insert %s: %v", name, err)
	}
}

func TestLoadEnvNoAgeKeys(t *testing.T) {
	t.Parallel()
	db := dbtest.New(t)

	oidc := appdb.NewOIDCStore(db)
	q := sqlc.New(db)
	ctx := context.Background()

	masterID, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}

	testDB := &vaultTestDB{oidc: oidc, q: q}
	svc, err := vault.NewService(testDB, masterID.String(), nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	user, err := oidc.CreateUser(ctx, auth.User{
		ID:    uuid.NewString(),
		Email: "nokeys2@vault.test",
		Name:  "No Keys 2",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	env, err := svc.LoadEnvForAgent(ctx, user.ID, "")
	if err != nil {
		t.Fatalf("LoadEnvForAgent: %v", err)
	}
	if len(env) != 0 {
		t.Fatalf("LoadEnv got %d entries, want 0", len(env))
	}
}

func TestDeleteEntry(t *testing.T) {
	t.Parallel()
	svc, _, userID := testService(t)
	ctx := context.Background()

	if err := svc.Set(ctx, userID, "MY_SECRET", "value"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if err := svc.Delete(ctx, userID, "MY_SECRET"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	entries, err := svc.List(ctx, userID)
	if err != nil {
		t.Fatalf("List after Delete: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("List after Delete: got %d entries, want 0", len(entries))
	}
}

func TestLoadEnvFiltersSystemManagedNames(t *testing.T) {
	svc, oidc, userID, q := testServiceWithQueries(t)
	ctx := context.Background()

	user, err := oidc.GetUser(ctx, userID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	svc.AddSystemManagedNames("CUSTOM_PROVIDER_BUNDLE")
	blocked := []string{
		"OAUTH_GITHUB_TOKEN",
		"MCP_TOKEN_01234567_89AB_4DEF_8123_456789ABCDEF",
		"GH_OAUTH",
		"CUSTOM_PROVIDER_BUNDLE",
		"STELLA_TOKEN",
	}
	for _, name := range blocked {
		insertUserVaultEntry(t, q, userID, user.AgePublicKey, name, "reserved-value")
	}
	if err := svc.Set(ctx, userID, "AMBIENT_KEY", "ambient-value"); err != nil {
		t.Fatalf("Set AMBIENT_KEY: %v", err)
	}

	env, err := svc.LoadEnvForAgent(ctx, userID, "agent-1")
	if err != nil {
		t.Fatalf("LoadEnvForAgent: %v", err)
	}
	if got := env["AMBIENT_KEY"]; got != "ambient-value" {
		t.Fatalf("AMBIENT_KEY = %q, want ambient-value", got)
	}
	for _, name := range blocked {
		if _, ok := env[name]; ok {
			t.Fatalf("%s should not be ambient", name)
		}
	}
}

func TestValidateUserFacingNameRejectsSystemManagedWithoutBlockingSystemWriters(t *testing.T) {
	svc, _, userID := testService(t)
	ctx := context.Background()
	svc.AddSystemManagedNames("CUSTOM_PROVIDER_BUNDLE")

	for _, name := range []string{"GH_OAUTH", "OAUTH_FOO", "MCP_TOKEN_01234567_89AB_4DEF_8123_456789ABCDEF", "MCP_OAUTH_01234567_89AB_4DEF_8123_456789ABCDEF", "CUSTOM_PROVIDER_BUNDLE"} {
		t.Run(name, func(t *testing.T) {
			err := svc.ValidateUserFacingName(name)
			if err == nil || !strings.Contains(err.Error(), "reserved for system-managed credentials") {
				t.Fatalf("ValidateUserFacingName(%q) = %v, want system-managed reserved error", name, err)
			}
		})
	}
	if err := vault.ValidateName(oauth.VaultKeyGitHub); err != nil {
		t.Fatalf("ValidateName(%q) = %v, want nil", oauth.VaultKeyGitHub, err)
	}
	if err := svc.Set(ctx, userID, oauth.VaultKeyGitHub, `{"access_token":"system"}`); err != nil {
		t.Fatalf("Set system-managed OAuth bundle through core Set: %v", err)
	}
}

func TestBuiltinOAuthVaultKeysAreNotAmbientWhenRegistryWired(t *testing.T) {
	svc, oidc, userID, q := testServiceWithQueries(t)
	ctx := context.Background()

	manifest, err := manifest.LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	registry := oauth.NewProviderRegistry()
	wantBlocked := make([]string, 0, len(manifest.OAuthProviders))
	for _, provider := range manifest.OAuthProviders {
		if provider.VaultKey == "" {
			continue
		}
		registry.Register(oauth.ProviderConfig{ID: provider.ID, VaultKey: provider.VaultKey})
		wantBlocked = append(wantBlocked, provider.VaultKey)
	}
	if len(wantBlocked) == 0 {
		t.Fatal("builtin manifest has no OAuth provider vault keys")
	}
	svc.AddSystemManagedNames(registry.VaultKeys()...)

	user, err := oidc.GetUser(ctx, userID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	for _, name := range wantBlocked {
		insertUserVaultEntry(t, q, userID, user.AgePublicKey, name, "provider-bundle")
	}
	if err := svc.Set(ctx, userID, "AMBIENT_KEY", "ambient-value"); err != nil {
		t.Fatalf("Set AMBIENT_KEY: %v", err)
	}

	env, err := svc.LoadEnvForAgent(ctx, userID, "agent-1")
	if err != nil {
		t.Fatalf("LoadEnvForAgent: %v", err)
	}
	if got := env["AMBIENT_KEY"]; got != "ambient-value" {
		t.Fatalf("AMBIENT_KEY = %q, want ambient-value", got)
	}
	for _, name := range wantBlocked {
		if _, ok := env[name]; ok {
			t.Fatalf("builtin OAuth vault key %s should not be ambient", name)
		}
	}
}

func TestMCPManagedOAuthPrefixIsNotAmbient(t *testing.T) {
	svc, _, userID, _ := testServiceWithQueries(t)
	ctx := context.Background()
	name := "MCP_OAUTH_0198F9A4_1B2C_7DEF_8123_456789ABCDEF"
	if err := svc.Set(ctx, userID, name, "provider-bundle"); err != nil {
		t.Fatalf("Set managed MCP OAuth bundle: %v", err)
	}
	if err := svc.Set(ctx, userID, "AMBIENT_KEY", "ambient-value"); err != nil {
		t.Fatalf("Set ambient key: %v", err)
	}

	env, err := svc.LoadEnvForAgent(ctx, userID, "agent-1")
	if err != nil {
		t.Fatalf("LoadEnvForAgent: %v", err)
	}
	if env["AMBIENT_KEY"] != "ambient-value" {
		t.Fatalf("AMBIENT_KEY = %q, want ambient-value", env["AMBIENT_KEY"])
	}
	if _, ok := env[name]; ok {
		t.Fatalf("managed MCP OAuth bundle %q leaked into ambient env", name)
	}
}
