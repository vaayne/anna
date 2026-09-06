package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/CherryHQ/stella/internal/auth"
	"github.com/CherryHQ/stella/internal/authz"
	appdb "github.com/CherryHQ/stella/internal/db"
	"github.com/CherryHQ/stella/internal/plugin"
)

func TestCommonConfigDeleteCleansOnlyAuthorizedCredentialFamily(t *testing.T) {
	svc, _, userID, _ := setupInternal(t)
	ctx := t.Context()
	configID, pluginID, _ := seedCommonConfig(t, svc.pool, userID, 1, AuthTypeOAuth)
	if _, err := svc.pool.Exec(ctx, `UPDATE plugin_config SET scope='system',user_id=NULL,config=jsonb_set(config,'{credential_mode}','"per_user"'),credential_refs='{}'::jsonb WHERE id=$1`, configID); err != nil {
		t.Fatal(err)
	}
	otherUser, err := appdb.NewOIDCStore(svc.pool).CreateUser(ctx, auth.User{ID: uuid.NewString(), Email: "other@cleanup.test", Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{userID, otherUser.ID} {
		for _, name := range []string{credentialName(configID), oauthBundleName(configID), oauthClientSecretName(configID)} {
			if _, err := svc.pool.Exec(ctx, `INSERT INTO vault_entry(id,scope,user_id,name,ciphertext) VALUES($1,'user',$2,$3,'synthetic')`, uuid.NewString(), owner, name); err != nil {
				t.Fatal(err)
			}
		}
	}
	unrelated := oauthBundleName(uuid.NewString())
	if _, err := svc.pool.Exec(ctx, `INSERT INTO vault_entry(id,scope,name,ciphertext) VALUES($1,'system',$2,'synthetic')`, uuid.NewString(), unrelated); err != nil {
		t.Fatal(err)
	}
	assertCounts := func(configs, credentials int) {
		t.Helper()
		var gotConfigs, gotCredentials int
		if err := svc.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM plugin_config WHERE id=$1),(SELECT count(*) FROM vault_entry)`, configID).Scan(&gotConfigs, &gotCredentials); err != nil {
			t.Fatal(err)
		}
		if gotConfigs != configs || gotCredentials != credentials {
			t.Fatalf("configs/credentials = %d/%d, want %d/%d", gotConfigs, gotCredentials, configs, credentials)
		}
	}
	admin, err := authz.NewUserAuthority(authz.UserID(userID), true)
	if err != nil {
		t.Fatal(err)
	}
	user, err := authz.NewUserAuthority(authz.UserID(userID), false)
	if err != nil {
		t.Fatal(err)
	}
	owner := CredentialOwner{Scope: ScopeUser, UserID: userID}
	if err := svc.DeleteCommonConfig(ctx, user, pluginID, configID, 1, owner); err == nil {
		t.Fatal("non-admin deleted system config")
	}
	assertCounts(1, 7)
	if err := svc.DeleteCommonConfig(ctx, admin, pluginID, configID, 2, owner); !errors.Is(err, plugin.ErrConflict) {
		t.Fatalf("stale CAS = %v", err)
	}
	assertCounts(1, 7)
	rollback := errors.New("force rollback after both deletes")
	err = svc.withCredentialMutationTx(ctx, admin, pluginID, configID, 1, owner, func(txCtx context.Context, access *plugin.Access, _ plugin.Config, mutation CredentialMutation) error {
		if err := mutation.DeleteAll(txCtx); err != nil {
			return err
		}
		if err := access.DeleteConfig(txCtx, pluginID, configID, 1); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("rollback seam = %v", err)
	}
	assertCounts(1, 7)
	access, err := svc.plugins.Begin(admin)
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	updated, err := access.UpdateConfig(ctx, pluginID, configID, 1, plugin.ConfigPatch{EnabledSet: true, Enabled: &disabled})
	if err != nil {
		t.Fatal(err)
	}
	assertCounts(1, 7)
	// Revoking local credentials must not depend on retaining the encryption key.
	svc.bindVault = nil
	if err := svc.DeleteCommonConfig(ctx, admin, pluginID, configID, updated.Revision, owner); err != nil {
		t.Fatal(err)
	}
	assertCounts(0, 1)
	var remaining string
	if err := svc.pool.QueryRow(ctx, `SELECT name FROM vault_entry`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != unrelated {
		t.Fatalf("remaining credential = %q", remaining)
	}
}
