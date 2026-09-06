package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CherryHQ/stella/internal/authz"
)

func TestCommonAuthNoneCRUDWorksWithoutVault(t *testing.T) {
	svc, _, userID, _ := setupInternal(t)
	svc.bindVault = nil
	configID := uuid.NewString()
	pluginID := "custom/" + configID
	namespace := "mcp_" + configID[:8]
	if _, err := svc.pool.Exec(t.Context(), `
		INSERT INTO plugin_definition(id, namespace, display_name, backend, source,
			implementation_key, spec, default_enabled, revision, creator_user_id)
		VALUES ($1, $2, $3, 'mcp', 'custom', 'mcp', '{}'::jsonb, false, 1, $4::uuid)`,
		pluginID, namespace, "Auth None", userID); err != nil {
		t.Fatalf("seed common definition: %v", err)
	}
	authority, err := authz.NewUserAuthority(authz.UserID(userID), true)
	if err != nil {
		t.Fatal(err)
	}
	ctx := authz.WithAuthority(context.Background(), authority)
	reg, err := svc.Create(ctx, CreateInput{
		PluginID: pluginID, Scope: ScopeUser, UserID: userID,
		Name: "auth-none", URL: "https://mcp.example.test",
		Transport: TransportStreamableHTTP, AuthType: AuthTypeNone,
	})
	if err != nil {
		t.Fatalf("Create without vault: %v", err)
	}
	if reg.ID == "" || reg.ConfigRevision < 1 {
		t.Fatalf("created registration = %#v", reg)
	}
	updatedURL := "https://mcp-updated.example.test"
	updated, err := svc.Update(ctx, UpdateInput{ID: reg.ID, Scope: ScopeUser, UserID: userID, URL: &updatedURL, ExpectedVersion: reg.Version()})
	if err != nil {
		t.Fatalf("Update without vault: %v", err)
	}
	if updated.URL != updatedURL {
		t.Fatalf("updated URL = %q, want %q", updated.URL, updatedURL)
	}
	if err := svc.Delete(ctx, updated.ID, ScopeUser, userID, ""); err != nil {
		t.Fatalf("Delete without vault: %v", err)
	}
	var count int
	if err := svc.pool.QueryRow(t.Context(), `SELECT count(*) FROM plugin_config WHERE id = $1::uuid`, reg.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("common config rows after delete = %d, want 0", count)
	}
}

func TestCommonUpdateRenamesDefinitionWithConfigCAS(t *testing.T) {
	svc, _, userID, _ := setupInternal(t)
	configID, pluginID, namespace := seedCommonConfig(t, svc.pool, userID, 1, AuthTypeNone)
	authority, err := authz.NewUserAuthority(authz.UserID(userID), true)
	if err != nil {
		t.Fatal(err)
	}
	ctx := authz.WithAuthority(t.Context(), authority)
	before, err := svc.Get(ctx, configID, ScopeUser, userID, "")
	if err != nil {
		t.Fatalf("get seeded config: %v", err)
	}
	newName := "Renamed MCP"
	updated, err := svc.Update(ctx, UpdateInput{
		ID: configID, Scope: ScopeUser, UserID: userID,
		Name: &newName, ExpectedVersion: before.Version(),
	})
	if err != nil {
		t.Fatalf("rename common config: %v", err)
	}
	if updated.Name != newName || updated.ID != configID || updated.PluginID != pluginID || updated.Namespace != namespace {
		t.Fatalf("renamed registration = %#v, want name %q with stable identity", updated, newName)
	}
	if updated.ConfigRevision != before.ConfigRevision+1 {
		t.Fatalf("config revision = %d, want %d after name-only update", updated.ConfigRevision, before.ConfigRevision+1)
	}
	var definitionName string
	var definitionRevision, configRevision int64
	if err := svc.pool.QueryRow(t.Context(), `SELECT display_name, revision FROM plugin_definition WHERE id = $1`, pluginID).Scan(&definitionName, &definitionRevision); err != nil {
		t.Fatalf("read renamed definition: %v", err)
	}
	if definitionName != newName || definitionRevision != 2 {
		t.Fatalf("definition = (%q, revision %d), want (%q, revision 2)", definitionName, definitionRevision, newName)
	}
	if err := svc.pool.QueryRow(t.Context(), `SELECT revision FROM plugin_config WHERE id = $1::uuid`, configID).Scan(&configRevision); err != nil {
		t.Fatalf("read config revision: %v", err)
	}
	if configRevision != updated.ConfigRevision {
		t.Fatalf("stored config revision = %d, returned %d", configRevision, updated.ConfigRevision)
	}
	if _, err := svc.Update(ctx, UpdateInput{
		ID: configID, Scope: ScopeUser, UserID: userID,
		Name: &newName, ExpectedVersion: before.Version(),
	}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale rename error = %v, want %v", err, ErrVersionConflict)
	}
}

func TestCommonTransportRejectsLegacyRegistrationBeforeCredentialRead(t *testing.T) {
	svc, _, userID, _ := setupInternal(t)
	configID, _, _ := seedCommonConfig(t, svc.pool, userID, 7, AuthTypeBearer)
	if err := svc.vault.SetScoped(t.Context(), ScopeUser, userID, "", credentialName(configID), "new-token"); err != nil {
		t.Fatal(err)
	}
	legacy := Registration{ID: configID, Scope: ScopeUser, UserID: userID, AuthType: AuthTypeBearer, CredentialRef: credentialName(configID), URL: "https://old.example.test", Transport: TransportStreamableHTTP}
	if _, err := svc.buildTransport(t.Context(), legacy, CredentialOwner{Scope: ScopeUser, UserID: userID}); !errors.Is(err, errPluginConfigIdentity) {
		t.Fatalf("legacy registration transport error = %v, want identity fence", err)
	}
}

func TestCommonCredentialSnapshotFencesRevision(t *testing.T) {
	svc, _, userID, _ := setupInternal(t)
	configID, pluginID, namespace := seedCommonConfig(t, svc.pool, userID, 7, AuthTypeBearer)
	reg := Registration{ID: configID, PluginID: pluginID, Namespace: namespace, ConfigRevision: 7, Scope: ScopeUser, UserID: userID, AuthType: AuthTypeBearer, CredentialRef: credentialName(configID)}
	if err := svc.vault.SetScoped(t.Context(), ScopeUser, userID, "", reg.CredentialRef, "old-token"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := svc.loadCredentialSnapshot(t.Context(), reg, CredentialOwner{Scope: ScopeUser, UserID: userID})
	if err != nil || snapshot.BearerToken != "old-token" {
		t.Fatalf("snapshot = %#v, err = %v", snapshot, err)
	}
	if _, err := svc.pool.Exec(t.Context(), `UPDATE plugin_config SET revision = 8 WHERE id = $1`, configID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.loadCredentialSnapshot(t.Context(), reg, CredentialOwner{Scope: ScopeUser, UserID: userID}); !errors.Is(err, errPluginConfigIdentity) {
		t.Fatalf("stale snapshot error = %v, want identity fence", err)
	}
}

func TestOAuthBundleCASFencesRevisionAndDigest(t *testing.T) {
	svc, _, userID, _ := setupInternal(t)
	configID, pluginID, namespace := seedCommonConfig(t, svc.pool, userID, 7, AuthTypeOAuth)
	reg := Registration{ID: configID, PluginID: pluginID, Namespace: namespace, ConfigRevision: 7, Scope: ScopeUser, UserID: userID, AuthType: AuthTypeOAuth, CredentialMode: CredentialModeShared}
	owner := CredentialOwner{Scope: ScopeUser, UserID: userID}
	old := OAuthBundle{Version: 1, ClientID: "client", TokenEndpoint: "https://issuer.example.test/token", AccessToken: "old", RefreshToken: "refresh"}
	oldRaw, _ := json.Marshal(old)
	if err := svc.vault.SetScoped(t.Context(), ScopeUser, userID, "", oauthBundleName(configID), string(oldRaw)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.pool.Exec(t.Context(), `UPDATE plugin_config SET revision = 8 WHERE id = $1`, configID); err != nil {
		t.Fatal(err)
	}
	if err := svc.storeBundleCAS(t.Context(), reg, owner, old, oldRaw); !errors.Is(err, errPluginConfigIdentity) {
		t.Fatalf("stale revision CAS error = %v, want identity fence", err)
	}
	if _, err := svc.pool.Exec(t.Context(), `UPDATE plugin_config SET revision = 7 WHERE id = $1`, configID); err != nil {
		t.Fatal(err)
	}
	newBundle := old
	newBundle.AccessToken = "newer"
	newRaw, _ := json.Marshal(newBundle)
	if err := svc.vault.SetScoped(t.Context(), ScopeUser, userID, "", oauthBundleName(configID), string(newRaw)); err != nil {
		t.Fatal(err)
	}
	if err := svc.storeBundleCAS(t.Context(), reg, owner, old, oldRaw); !errors.Is(err, errOAuthBundleChanged) {
		t.Fatalf("same-revision digest CAS error = %v, want bundle conflict", err)
	}
}

func TestOAuthCallbackRevisionFence(t *testing.T) {
	svc, _, userID, _ := setupInternal(t)
	configID, pluginID, namespace := seedCommonConfig(t, svc.pool, userID, 3, AuthTypeOAuth)
	reg := Registration{ID: configID, PluginID: pluginID, Namespace: namespace, ConfigRevision: 3, Scope: ScopeUser, UserID: userID, AuthType: AuthTypeOAuth, CredentialMode: CredentialModeShared}
	if _, err := svc.pool.Exec(t.Context(), `UPDATE plugin_config SET revision = 4 WHERE id = $1`, configID); err != nil {
		t.Fatal(err)
	}
	if err := svc.validatePluginConfigRegistration(t.Context(), reg); !errors.Is(err, errPluginConfigIdentity) {
		t.Fatalf("stale OAuth callback fence = %v, want identity fence", err)
	}
}

func seedCommonConfig(t *testing.T, pool *pgxpool.Pool, userID string, revision int64, authType string) (string, string, string) {
	t.Helper()
	configID := uuid.NewString()
	pluginID := "custom/" + configID
	namespace := "mcp_" + configID[:8]
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO plugin_definition(id, namespace, display_name, backend, source, implementation_key, spec, default_enabled, revision, creator_user_id) VALUES($1,$2,$2,'mcp','custom','mcp','{}',false,1,$3)`, pluginID, namespace, userID); err != nil {
		t.Fatal(err)
	}
	payload := `{"url":"https://mcp.example.test","transport":"streamable_http","auth_type":"` + authType + `"}`
	refs := `{}`
	if authType == AuthTypeBearer {
		refs = `{"bearer":{"name":"` + credentialName(configID) + `","scope":"user","user_id":"` + userID + `","agent_id":""}}`
	}
	if authType == AuthTypeOAuth {
		refs = `{"oauth_bundle":{"name":"` + oauthBundleName(configID) + `","scope":"user","user_id":"` + userID + `","agent_id":""}}`
	}
	if _, err := pool.Exec(ctx, `INSERT INTO plugin_config(id,plugin_id,namespace,scope,user_id,enabled,config,credential_refs,revision) VALUES($1,$2,$3,'user',$4,true,$5::jsonb,$6::jsonb,$7)`, configID, pluginID, namespace, userID, payload, refs, revision); err != nil {
		t.Fatal(err)
	}
	return configID, pluginID, namespace
}
