package mcp

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/plugin"
)

func TestCommonMCPPolicyRejectsRawIdentityMutationAndCleansDelete(t *testing.T) {
	svc, _, userID, _ := setupInternal(t)
	configID, pluginID, _ := seedCommonConfig(t, svc.pool, userID, 1, AuthTypeOAuth)
	authority, err := authz.NewUserAuthority(authz.UserID(userID), true)
	if err != nil {
		t.Fatal(err)
	}
	access, err := svc.plugins.Begin(authority)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	current, err := access.GetConfig(ctx, pluginID, configID)
	if err != nil {
		t.Fatal(err)
	}
	grant := oauthBundleName(configID)
	if _, err := svc.pool.Exec(ctx, `INSERT INTO vault_entry(id,scope,user_id,name,ciphertext) VALUES($1,'user',$2,$3,'synthetic')`, uuid.NewString(), userID, grant); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(current.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload["url"] = "https://new-endpoint.example/mcp"
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := access.UpdateConfig(ctx, pluginID, configID, 1, plugin.ConfigPatch{PayloadSet: true, Payload: raw}); !errors.Is(err, errTypedConfigMutationRequired) {
		t.Fatalf("raw endpoint edit = %v", err)
	}
	unchanged, err := access.GetConfig(ctx, pluginID, configID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Revision != 1 {
		t.Fatalf("rejected edit changed revision: %d", unchanged.Revision)
	}
	if err := access.DeleteConfig(ctx, pluginID, configID, 2); !errors.Is(err, plugin.ErrConflict) {
		t.Fatalf("stale delete = %v", err)
	}
	var count int
	if err := svc.pool.QueryRow(ctx, `SELECT count(*) FROM vault_entry WHERE name=$1`, grant).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("rejected edit/delete changed grant")
	}
	if err := access.DeleteConfig(ctx, pluginID, configID, 1); err != nil {
		t.Fatal(err)
	}
	if err := svc.pool.QueryRow(ctx, `SELECT count(*) FROM vault_entry WHERE name=$1`, grant).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("generic delete orphaned grant")
	}
}

func TestTypedMCPPolicySameLocatorReplacementRollsBackAndRevokes(t *testing.T) {
	svc, _, userID, _ := setupInternal(t)
	authority, err := authz.NewUserAuthority(authz.UserID(userID), false)
	if err != nil {
		t.Fatal(err)
	}
	ctx := authz.WithAuthority(t.Context(), authority)
	_, cfg, err := svc.CreateCustom(ctx, customMCPDefinition("rotation"), CreateInput{Scope: ScopeUser, Name: "rotation", URL: "https://mcp.example.test", Transport: TransportStreamableHTTP, AuthType: AuthTypeOAuth, OAuthClientID: "client", OAuthClientSecret: "old-secret"})
	if err != nil {
		t.Fatal(err)
	}
	grant := oauthBundleName(cfg.ID)
	if _, err := svc.pool.Exec(ctx, `INSERT INTO vault_entry(id,scope,user_id,name,ciphertext) VALUES($1,'user',$2,$3,'synthetic')`, uuid.NewString(), userID, grant); err != nil {
		t.Fatal(err)
	}
	reg, err := svc.Get(ctx, cfg.ID, ScopeUser, userID, "")
	if err != nil {
		t.Fatal(err)
	}
	secret := "new-secret"
	originalBind := svc.bindVault
	svc.bindVault = nil
	_, err = svc.UpdateIfVersion(ctx, UpdateInput{ID: cfg.ID, Scope: ScopeUser, UserID: userID, OAuthClientSecret: &secret}, reg.Version())
	if !errors.Is(err, errPluginCredentialsUnavailable) {
		t.Fatalf("failed replacement = %v", err)
	}
	svc.bindVault = originalBind
	assertGrant := func(want int) {
		t.Helper()
		var got int
		if err := svc.pool.QueryRow(ctx, `SELECT count(*) FROM vault_entry WHERE name=$1`, grant).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("grant count=%d want=%d", got, want)
		}
	}
	assertGrant(1)
	afterFailure, err := svc.Get(ctx, cfg.ID, ScopeUser, userID, "")
	if err != nil {
		t.Fatal(err)
	}
	if afterFailure.ConfigRevision != reg.ConfigRevision {
		t.Fatal("failed store did not roll back config")
	}
	if got, err := svc.vault.GetScoped(ctx, ScopeUser, userID, "", oauthClientSecretName(cfg.ID)); err != nil || got != "old-secret" {
		t.Fatalf("old secret not restored: %v", err)
	}
	disabled := false
	reg, err = svc.UpdateIfVersion(ctx, UpdateInput{ID: cfg.ID, Scope: ScopeUser, UserID: userID, Enabled: &disabled}, reg.Version())
	if err != nil {
		t.Fatal(err)
	}
	assertGrant(1)
	_, err = svc.UpdateIfVersion(ctx, UpdateInput{ID: cfg.ID, Scope: ScopeUser, UserID: userID, OAuthClientSecret: &secret}, reg.Version())
	if err != nil {
		t.Fatal(err)
	}
	assertGrant(0)
	if got, err := svc.vault.GetScoped(ctx, ScopeUser, userID, "", oauthClientSecretName(cfg.ID)); err != nil || got != "new-secret" {
		t.Fatalf("new secret missing: %v", err)
	}
}
