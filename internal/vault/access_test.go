package vault_test

import (
	"context"
	"errors"
	"testing"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	storepkg "github.com/CherryHQ/stella/cmd/stellad/store"
	"github.com/CherryHQ/stella/internal/auth"
	"github.com/CherryHQ/stella/internal/authz"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	appdb "github.com/CherryHQ/stella/internal/db"
	"github.com/CherryHQ/stella/internal/db/dbtest"
	"github.com/CherryHQ/stella/internal/vault"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
)

func userAuthority(t *testing.T, id string) authz.Authority {
	t.Helper()
	return mintUserAuthority(t, id, false)
}

func adminAuthority(t *testing.T, id string) authz.Authority {
	t.Helper()
	return mintUserAuthority(t, id, true)
}

func mintUserAuthority(t *testing.T, id string, admin bool) authz.Authority {
	t.Helper()
	a, err := authz.NewUserAuthority(authz.UserID(id), admin)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// vaultPEP builds a PEP-enabled vault Service over a fresh pool, returning the
// pool so the service and its agent gate share the same durable test state,
// and a seeded owner user with age keys provisioned.
func vaultPEP(t *testing.T) (*vault.Service, *pgxpool.Pool, string) {
	t.Helper()
	db := dbtest.New(t)
	ctx := context.Background()
	oidc := appdb.NewOIDCStore(db)
	masterID, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}
	testDB := &vaultTestDB{oidc: oidc, q: sqlc.New(db)}
	agents := agentaccess.NewService(storepkg.NewDBStore(db), appdb.NewAuthStore(db))
	svc, err := vault.NewService(testDB, masterID.String(), agents)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	owner, err := oidc.CreateUser(ctx, auth.User{ID: uuid.NewString(), Email: "owner@vault.test", Role: auth.RoleUser})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	pub, encPriv, err := vault.GenerateUserKeys(svc.MasterRecipient())
	if err != nil {
		t.Fatalf("GenerateUserKeys: %v", err)
	}
	if err := oidc.UpdateUserAgeKeys(ctx, owner.ID, pub, encPriv); err != nil {
		t.Fatalf("UpdateUserAgeKeys: %v", err)
	}
	return svc, db, owner.ID
}

// The owner can round-trip a user secret; a foreign user is denied; admin holds
// the admin-managed system scope; a non-admin cannot.
func TestVaultAccessOwnerAdminAndForeign(t *testing.T) {
	svc, _, ownerID := vaultPEP(t)
	ctx := context.Background()
	begin := func(authority authz.Authority) *vault.Access {
		acc, err := svc.Begin(ctx, authority)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		return acc
	}

	if err := begin(userAuthority(t, ownerID)).SetScoped(ctx, vault.ScopeUser, "", "MY_SECRET", "v", vault.SetOptions{}); err != nil {
		t.Fatalf("owner Set: %v", err)
	}
	if got, err := begin(userAuthority(t, ownerID)).GetScoped(ctx, vault.ScopeUser, "", "MY_SECRET"); err != nil || got != "v" {
		t.Fatalf("owner Get = %q, %v; want v", got, err)
	}

	// A foreign user cannot read the owner's user scope (different owner column).
	if _, err := begin(userAuthority(t, "foreign")).GetScoped(ctx, vault.ScopeUser, "", "MY_SECRET"); err == nil {
		t.Fatal("foreign Get succeeded, want denial/not-found")
	}

	// A non-admin user cannot write a system scope; an admin can.
	if err := begin(userAuthority(t, ownerID)).SetScoped(ctx, vault.ScopeSystem, "", "SYS", "s", vault.SetOptions{}); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("non-admin system Set err=%v, want forbidden", err)
	}
	if err := begin(adminAuthority(t, "admin-1")).SetScoped(ctx, vault.ScopeSystem, "", "SYS", "s", vault.SetOptions{}); err != nil {
		t.Fatalf("admin system Set: %v", err)
	}
	if got, err := begin(adminAuthority(t, "admin-1")).GetScoped(ctx, vault.ScopeSystem, "", "SYS"); err != nil || got != "s" {
		t.Fatalf("admin system Get = %q, %v; want s", got, err)
	}
}

func TestVaultAccessHidesManagedMCPOAuthEntries(t *testing.T) {
	svc, _, ownerID := vaultPEP(t)
	ctx := context.Background()
	managedNames := []string{
		"MCP_OAUTH_0198F9A4_1B2C_7DEF_8123_456789ABCDEF",
		"MCP_OAUTH_CLIENT_0198F9A4_1B2C_7DEF_8123_456789ABCDEF",
	}
	for _, name := range managedNames {
		if err := svc.Set(ctx, ownerID, name, "managed-value"); err != nil {
			t.Fatalf("trusted Set %q: %v", name, err)
		}
	}
	if err := svc.Set(ctx, ownerID, "VISIBLE_SECRET", "visible-value"); err != nil {
		t.Fatalf("trusted Set visible: %v", err)
	}

	access, err := svc.Begin(ctx, userAuthority(t, ownerID))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	entries, err := access.ListScoped(ctx, vault.ScopeUser, "")
	if err != nil {
		t.Fatalf("ListScoped: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "VISIBLE_SECRET" {
		t.Fatalf("ListScoped = %+v, managed names must be hidden", entries)
	}
	for _, name := range managedNames {
		if _, err := access.GetScoped(ctx, vault.ScopeUser, "", name); !errors.Is(err, authz.ErrForbidden) {
			t.Fatalf("GetScoped(%q) = %v, want forbidden", name, err)
		}
		if _, err := access.GetScopedMeta(ctx, vault.ScopeUser, "", name); !errors.Is(err, authz.ErrForbidden) {
			t.Fatalf("GetScopedMeta(%q) = %v, want forbidden", name, err)
		}
		if err := access.SetScoped(ctx, vault.ScopeUser, "", name, "user-value", vault.SetOptions{}); !errors.Is(err, authz.ErrForbidden) {
			t.Fatalf("SetScoped(%q) = %v, want forbidden", name, err)
		}
		if err := access.DeleteScoped(ctx, vault.ScopeUser, "", name); !errors.Is(err, authz.ErrForbidden) {
			t.Fatalf("DeleteScoped(%q) = %v, want forbidden", name, err)
		}
		if got, err := svc.Get(ctx, ownerID, name); err != nil || got != "managed-value" {
			t.Fatalf("trusted Get(%q) = %q, %v; raw trusted access must remain available", name, got, err)
		}
	}
}
