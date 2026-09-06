// Package dbtest gives tests an isolated, fully-migrated PostgreSQL database
// with no external server to run. It starts one embedded server per test binary,
// migrates a template once, and hands each test a fresh database cloned from
// that template — real commits and all, so concurrency behaviour (advisory
// locks, transactions) is exercised exactly as in production.
//
// Use it from a test package like:
//
//	func TestMain(m *testing.M) { dbtest.Main(m) }
//
//	func TestThing(t *testing.T) {
//		db := dbtest.New(t)
//		// ... db is a clean, migrated *pgxpool.Pool; dropped when the test ends
//	}
package dbtest

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

	appdb "github.com/CherryHQ/stella/internal/db"
)

// templateDB is migrated once per process; every test database is a fast
// file-copy clone of it (CREATE DATABASE ... TEMPLATE), so migrations run once
// instead of once per test.
const templateDB = "stella_tmpl"

var (
	once    sync.Once
	server  *appdb.Embedded
	admin   *pgxpool.Pool // maintenance pool on the "postgres" database
	initErr error
	seq     atomic.Int64
)

// ensure lazily boots the shared server and builds the migrated template. It runs
// at most once per process; the first New caller pays the startup cost.
func ensure() {
	once.Do(func() {
		server, initErr = appdb.StartEmbedded("", 0)
		if initErr != nil {
			return
		}

		admin, initErr = pgxpool.New(context.Background(), server.DSNFor("postgres"))
		if initErr != nil {
			return
		}

		// Migrate a throwaway database, then close every connection to it: a
		// template must be quiescent before it can seed CREATE DATABASE clones.
		if _, err := admin.Exec(context.Background(), "CREATE DATABASE "+templateDB); err != nil {
			initErr = fmt.Errorf("dbtest: create template: %w", err)
			return
		}
		tmpl, err := appdb.OpenDB(server.DSNFor(templateDB), appdb.WithMaxConns(4))
		if err != nil {
			initErr = fmt.Errorf("dbtest: migrate template: %w", err)
			return
		}
		tmpl.Close()
	})
}

// New returns a fresh, isolated, fully-migrated database for the calling test and
// registers its teardown. Each call clones the shared template, so tests neither
// see each other's data nor re-run migrations.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ensure()
	if initErr != nil {
		t.Fatalf("dbtest: %v", initErr)
	}

	ctx := context.Background()
	name := fmt.Sprintf("test_%d", seq.Add(1))
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, templateDB)); err != nil {
		t.Fatalf("dbtest: create %s: %v", name, err)
	}

	// Open through OpenDB so the test handle is configured exactly like the
	// server's: same pool policy and FTS guarantees. The clone is already
	// migrated, so OpenDB's migrate step is a no-op.
	db, err := appdb.OpenDB(server.DSNFor(name), appdb.WithMaxConns(4))
	if err != nil {
		t.Fatalf("dbtest: open %s: %v", name, err)
	}

	t.Cleanup(func() {
		db.Close()
		// Drop the database; terminate any lingering backends first so the drop
		// can't be blocked by a leaked connection.
		_, _ = admin.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", name)
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name)
	})
	return db
}

// NewAtMigration returns a fresh database whose goose ledger ends at version.
// It is for integration fixtures that exercise a migration boundary; unlike
// New, it deliberately starts from an empty database so a later migration's
// destructive data changes cannot be mistaken for a historical schema.
func NewAtMigration(t *testing.T, version int64) *pgxpool.Pool {
	t.Helper()
	ensure()
	if initErr != nil {
		t.Fatalf("dbtest: %v", initErr)
	}

	ctx := context.Background()
	name := fmt.Sprintf("test_history_%d", seq.Add(1))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("dbtest: create %s: %v", name, err)
	}

	db, err := pgxpool.New(ctx, server.DSNFor(name))
	if err != nil {
		t.Fatalf("dbtest: open %s: %v", name, err)
	}
	t.Cleanup(func() {
		db.Close()
		_, _ = admin.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", name)
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name)
	})

	conn, err := db.Acquire(ctx)
	if err != nil {
		t.Fatalf("dbtest: acquire %s: %v", name, err)
	}
	for _, extension := range []string{"vector", "pg_search"} {
		if _, err := conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS "+extension); err != nil {
			conn.Release()
			t.Fatalf("dbtest: create extension %s: %v", extension, err)
		}
	}
	conn.Release()

	migrations, err := fs.Sub(appdb.MigrationsFS, "migrations")
	if err != nil {
		t.Fatalf("dbtest: open migrations: %v", err)
	}
	sqlDB := stdlib.OpenDBFromPool(db)
	t.Cleanup(func() { _ = sqlDB.Close() })
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations)
	if err != nil {
		t.Fatalf("dbtest: create migration provider: %v", err)
	}
	if _, err := provider.UpTo(ctx, version); err != nil {
		t.Fatalf("dbtest: migrate %s to %d: %v", name, version, err)
	}
	return db
}

// Main runs a package's tests against the shared embedded server and stops it
// afterward. Test packages that use New call this from TestMain so the server
// process and its data directory are cleaned up when the binary exits.
func Main(m *testing.M) {
	code := m.Run()
	if server != nil {
		_ = server.Stop()
	}
	os.Exit(code)
}
