package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
)

func TestPluginImportBoundsInitialCatalogLock(t *testing.T) {
	db := newTestDB(t)
	held, err := db.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Rollback(t.Context()) }()
	if err := sqlc.New(held).LockPluginCatalog(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err = plugin.ImportLegacyState(ctx, db, plugin.NewCatalog(), nil)
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) || databaseError.Code != "55P03" {
		t.Fatalf("initial lock error = %v, want PostgreSQL lock timeout before context cancellation", err)
	}
}
