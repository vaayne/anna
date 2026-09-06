package vault_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/CherryHQ/stella/internal/auth"
	appdb "github.com/CherryHQ/stella/internal/db"
	"github.com/CherryHQ/stella/internal/db/dbtest"
	"github.com/CherryHQ/stella/internal/vault"
)

func TestMCPConfigCredentialCleanupIsExactAndTransactionalWithoutKey(t *testing.T) {
	db := dbtest.New(t)
	ctx := t.Context()
	oidc := appdb.NewOIDCStore(db)
	var users []string
	for _, label := range []string{"a", "b"} {
		user, err := oidc.CreateUser(ctx, auth.User{ID: uuid.NewString(), Email: label + "@cleanup.test", Name: label})
		if err != nil {
			t.Fatal(err)
		}
		users = append(users, user.ID)
	}
	target := uuid.New()
	other := uuid.New()
	var names []string
	for _, prefix := range []string{"MCP_TOKEN_", "MCP_OAUTH_", "MCP_OAUTH_CLIENT_"} {
		names = append(names, prefix+strings.ToUpper(strings.ReplaceAll(target.String(), "-", "_")))
	}
	untouched := "MCP_OAUTH_" + strings.ToUpper(strings.ReplaceAll(other.String(), "-", "_"))
	names = append(names, untouched, names[1]+"_SUFFIX", "ORDINARY_SECRET")
	for _, userID := range users {
		for _, name := range names {
			// Synthetic ciphertext proves deletion needs neither reading nor decryption.
			if _, err := db.Exec(ctx, `INSERT INTO vault_entry (id,scope,user_id,name,ciphertext) VALUES ($1,'user',$2,$3,'synthetic')`, uuid.NewString(), userID, name); err != nil {
				t.Fatal(err)
			}
		}
	}
	count := func(want int) {
		t.Helper()
		var got int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM vault_entry`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("credential rows = %d, want %d", got, want)
		}
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := vault.DeleteMCPConfigCredentialsTx(ctx, tx, target); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM vault_entry`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 6 {
		t.Fatalf("remaining = %d, want unrelated and suffix names preserved", remaining)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	count(12)
	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := vault.DeleteMCPConfigCredentialsTx(ctx, tx, target); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	count(6)
}
