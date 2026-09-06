package db

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// Package db's own tests cannot use internal/db/dbtest — that helper imports this
// package — so this mirrors it locally: one embedded server per test binary, a
// template migrated once, and a fresh cloned database per test via
// CREATE DATABASE ... TEMPLATE.
var (
	pkgTestOnce   sync.Once
	pkgTestServer *Embedded
	pkgTestAdmin  *pgxpool.Pool
	pkgTestErr    error
	pkgTestSeq    atomic.Int64
)

const pkgTestTemplate = "stella_tmpl"

func pkgTestEnsure() {
	pkgTestOnce.Do(func() {
		pkgTestServer, pkgTestErr = StartEmbedded("", 0)
		if pkgTestErr != nil {
			return
		}
		pkgTestAdmin, pkgTestErr = pgxpool.New(context.Background(), pkgTestServer.DSNFor("postgres"))
		if pkgTestErr != nil {
			return
		}
		if _, err := pkgTestAdmin.Exec(context.Background(), "CREATE DATABASE "+pkgTestTemplate); err != nil {
			pkgTestErr = fmt.Errorf("create template: %w", err)
			return
		}
		tmpl, err := OpenDB(pkgTestServer.DSNFor(pkgTestTemplate), WithMaxConns(4))
		if err != nil {
			pkgTestErr = fmt.Errorf("migrate template: %w", err)
			return
		}
		tmpl.Close()
	})
}

// newTestDB returns a fresh, fully-migrated, isolated database for one test,
// dropped when the test ends.
func newTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pkgTestEnsure()
	if pkgTestErr != nil {
		t.Fatalf("newTestDB: %v", pkgTestErr)
	}
	name := fmt.Sprintf("dbtest_%d", pkgTestSeq.Add(1))
	if _, err := pkgTestAdmin.Exec(context.Background(), fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, pkgTestTemplate)); err != nil {
		t.Fatalf("newTestDB: create %s: %v", name, err)
	}
	db, err := OpenDB(pkgTestServer.DSNFor(name), WithMaxConns(4))
	if err != nil {
		t.Fatalf("newTestDB: open %s: %v", name, err)
	}
	t.Cleanup(func() {
		db.Close()
		_, _ = pkgTestAdmin.Exec(context.Background(), "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", name)
		_, _ = pkgTestAdmin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name)
	})
	return db
}

// newTestDBAtMigration creates an empty database and advances it to the
// requested historical boundary. Down migrations are not a schema
// reconstruction mechanism, and migration 41 deliberately cannot be rolled
// back after the runtime identity cutover.
func newTestDBAtMigration(t *testing.T, version int64) (*pgxpool.Pool, *goose.Provider) {
	t.Helper()
	pkgTestEnsure()
	if pkgTestErr != nil {
		t.Fatalf("newTestDBAtMigration: %v", pkgTestErr)
	}
	name := fmt.Sprintf("dbtest_history_%d", pkgTestSeq.Add(1))
	if _, err := pkgTestAdmin.Exec(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("newTestDBAtMigration: create %s: %v", name, err)
	}
	db, err := pgxpool.New(context.Background(), pkgTestServer.DSNFor(name))
	if err != nil {
		t.Fatalf("newTestDBAtMigration: open %s: %v", name, err)
	}
	t.Cleanup(func() {
		db.Close()
		_, _ = pkgTestAdmin.Exec(context.Background(), "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", name)
		_, _ = pkgTestAdmin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name)
	})

	conn, err := db.Acquire(context.Background())
	if err != nil {
		t.Fatalf("newTestDBAtMigration: acquire %s: %v", name, err)
	}
	err = ensureExtensions(context.Background(), conn)
	conn.Release()
	if err != nil {
		t.Fatalf("newTestDBAtMigration: extensions %s: %v", name, err)
	}

	migrations, err := fs.Sub(MigrationsFS, "migrations")
	if err != nil {
		t.Fatalf("newTestDBAtMigration: open migrations: %v", err)
	}
	sqlDB := stdlib.OpenDBFromPool(db)
	t.Cleanup(func() { _ = sqlDB.Close() })
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations)
	if err != nil {
		t.Fatalf("newTestDBAtMigration: create provider: %v", err)
	}
	if _, err := provider.UpTo(context.Background(), version); err != nil {
		t.Fatalf("newTestDBAtMigration: migrate to %d: %v", version, err)
	}
	return db, provider
}

// newTestDBAtMigrationOnly is the historical equivalent of newTestDB for
// tests that exercise the import boundary. The importer is intentionally
// authoritative only at migration 41, before the post-import retirement pass.
func newTestDBAtMigrationOnly(t *testing.T, version int64) *pgxpool.Pool {
	t.Helper()
	db, _ := newTestDBAtMigration(t, version)
	return db
}

func TestMain(m *testing.M) {
	code := m.Run()
	if pkgTestServer != nil {
		_ = pkgTestServer.Stop()
	}
	os.Exit(code)
}
