package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CherryHQ/stella/internal/platform/home"
)

const projectBaseDirMigration = sequentialAnchor + 16

func TestProjectBaseDirMigrationCanonicalizesPhysicalOwnerPaths(t *testing.T) {
	db, provider := newTestDBAtMigration(t, projectBaseDirMigration-1)
	ctx := t.Context()

	userID := uuid.NewString()
	agentID := "project-coordinate-agent"
	seedProjectBaseDirOwners(t, ctx, db, userID, agentID)
	homeDir := t.TempDir()
	ownerRoot := filepath.Join(homeDir, "users", userID, "agents", agentID)
	if err := os.MkdirAll(ownerRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "project-alias")
	if err := os.Symlink(ownerRoot, alias); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, stored, want string
	}{
		{name: "canonical root", stored: ".", want: "."},
		{name: "canonical nested", stored: "nested/project", want: "nested/project"},
		{name: "POSIX owner root", stored: ownerRoot, want: "."},
		{name: "POSIX nested", stored: ownerRoot + "/projects/app", want: "projects/app"},
		{name: "POSIX owner root trailing separator", stored: ownerRoot + "/", want: "."},
		{name: "POSIX repeated separator", stored: ownerRoot + "//projects///app", want: "projects/app"},
		{name: "POSIX parent component", stored: ownerRoot + "/projects/../app", want: "app"},
		{name: "physical symlink alias", stored: filepath.Join(alias, "aliased", "missing"), want: "aliased/missing"},
	}
	ids := make(map[string]string, len(cases))
	for _, tc := range cases {
		id := uuid.NewString()
		ids[tc.name] = id
		if _, err := db.Exec(ctx, `
			INSERT INTO project (id, agent_id, user_id, name, base_dir)
			VALUES ($1, $2, $3, $4, $5)
		`, id, agentID, userID, tc.name, tc.stored); err != nil {
			t.Fatalf("seed %s: %v", tc.name, err)
		}
	}

	if _, err := provider.UpTo(ctx, projectBaseDirMigration); err != nil {
		t.Fatalf("migrate project coordinates: %v", err)
	}
	assertProjectCoordinateConstraintValidated(t, ctx, db, false)
	manager, err := home.NewWorkspaceManager(db, homeDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if err := reconcileProjectCoordinatesStrict(ctx, manager); err != nil {
		t.Fatalf("finish filesystem-aware project coordinate migration: %v", err)
	}
	if err := reconcileProjectCoordinatesStrict(ctx, manager); err != nil {
		t.Fatalf("repeat completed project coordinate migration: %v", err)
	}
	assertProjectCoordinateConstraintValidated(t, ctx, db, true)
	for _, tc := range cases {
		var got string
		if err := db.QueryRow(ctx, `SELECT base_dir FROM project WHERE id = $1`, ids[tc.name]).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s base_dir = %q, want %q", tc.name, got, tc.want)
		}
	}

	for index, invalid := range []string{"", "/outside/project", `C:\outside\project`, "../traversal", "nested//project", "nested/./project", "nested/project/", "project:name", "$HOME/project", "$OTHER/project", "control" + string(rune(0x1f)) + "path"} {
		if _, err := db.Exec(ctx, `
			INSERT INTO project (id, agent_id, user_id, name, base_dir)
			VALUES ($1, $2, $3, $4, $5)
		`, uuid.NewString(), agentID, userID, fmt.Sprintf("invalid-%d", index), invalid); err == nil {
			t.Errorf("canonical constraint accepted %q", invalid)
		}
	}
}

func TestProjectBaseDirStartupReconcileUpdatesSafeRowsAndIsolatesAmbiguousRows(t *testing.T) {
	db, provider := newTestDBAtMigration(t, projectBaseDirMigration-1)
	ctx := t.Context()

	userID := uuid.NewString()
	agentID := "startup-project-coordinate-agent"
	seedProjectBaseDirOwners(t, ctx, db, userID, agentID)
	homeDir := t.TempDir()
	ownerRoot := filepath.Join(homeDir, "users", userID, "agents", agentID)
	target := filepath.Join(ownerRoot, "repos", "stella")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	safeID, copiedID, copiedRootID, ambiguousID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	legacy := filepath.Join(homeDir, "workspaces", agentID, "users", userID, "repos", "stella")
	legacyRoot := filepath.Join(homeDir, "workspaces", agentID, "users", userID)
	copiedLegacy := filepath.Join(homeDir, "workspaces", agentID, "users", userID, "repos", "copied")
	if err := os.MkdirAll(copiedLegacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ownerRoot, "repos", "copied"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO project (id, agent_id, user_id, name, base_dir) VALUES
		($1, $5, $6, 'safe legacy', $7),
		($2, $5, $6, 'distinct copied trees', $8),
		($3, $5, $6, 'distinct copied roots', $9),
		($4, $5, $6, 'ambiguous legacy', '/srv/outside/project')
	`, safeID, copiedID, copiedRootID, ambiguousID, agentID, userID, legacy, copiedLegacy, legacyRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, projectBaseDirMigration); err != nil {
		t.Fatal(err)
	}
	manager, err := home.NewWorkspaceManager(db, homeDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	result, err := manager.ReconcileProjectCoordinates(ctx)
	if err != nil {
		t.Fatalf("startup reconciliation: %v", err)
	}
	wantUnresolved := []string{copiedID, copiedRootID, ambiguousID}
	slices.Sort(wantUnresolved)
	if result.Updated != 1 || !slices.Equal(result.UnresolvedIDs, wantUnresolved) {
		t.Fatalf("startup reconciliation result = %#v", result)
	}
	var safe, copied, copiedRoot, ambiguous string
	if err := db.QueryRow(ctx, "SELECT base_dir FROM project WHERE id=$1", safeID).Scan(&safe); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, "SELECT base_dir FROM project WHERE id=$1", ambiguousID).Scan(&ambiguous); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, "SELECT base_dir FROM project WHERE id=$1", copiedID).Scan(&copied); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, "SELECT base_dir FROM project WHERE id=$1", copiedRootID).Scan(&copiedRoot); err != nil {
		t.Fatal(err)
	}
	if safe != "repos/stella" || copied != copiedLegacy || copiedRoot != legacyRoot || ambiguous != "/srv/outside/project" {
		t.Fatalf("reconciled coordinates safe=%q copied=%q copiedRoot=%q ambiguous=%q", safe, copied, copiedRoot, ambiguous)
	}
	assertProjectCoordinateConstraintValidated(t, ctx, db, false)
}

func TestProjectBaseDirMigrationToleratesConcurrentCanonicalizationAndDeletion(t *testing.T) {
	for _, tc := range []struct {
		name       string
		concurrent func(context.Context, *testing.T, pgx.Tx, string)
		wantValue  string
		wantRow    bool
	}{
		{
			name: "canonicalization",
			concurrent: func(ctx context.Context, t *testing.T, tx pgx.Tx, id string) {
				t.Helper()
				if _, err := tx.Exec(ctx, `UPDATE project SET base_dir = 'concurrent/project' WHERE id = $1`, id); err != nil {
					t.Fatalf("concurrently canonicalize project: %v", err)
				}
			},
			wantValue: "concurrent/project",
			wantRow:   true,
		},
		{
			name: "deletion",
			concurrent: func(ctx context.Context, t *testing.T, tx pgx.Tx, id string) {
				t.Helper()
				if _, err := tx.Exec(ctx, `DELETE FROM project WHERE id = $1`, id); err != nil {
					t.Fatalf("concurrently delete project: %v", err)
				}
			},
			wantRow: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, provider := newTestDBAtMigration(t, projectBaseDirMigration-1)
			ctx := t.Context()

			userID := uuid.NewString()
			agentID := "concurrent-project-coordinate-agent"
			seedProjectBaseDirOwners(t, ctx, db, userID, agentID)
			homeDir := t.TempDir()
			ownerRoot := filepath.Join(homeDir, "users", userID, "agents", agentID)
			if err := os.MkdirAll(ownerRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			id := uuid.NewString()
			if _, err := db.Exec(ctx, `
				INSERT INTO project (id, agent_id, user_id, name, base_dir)
				VALUES ($1, $2, $3, 'concurrent-project', $4)
			`, id, agentID, userID, filepath.Join(ownerRoot, "historical")); err != nil {
				t.Fatalf("seed concurrent project: %v", err)
			}
			if _, err := provider.UpTo(ctx, projectBaseDirMigration); err != nil {
				t.Fatalf("install pending project coordinate migration: %v", err)
			}
			assertProjectCoordinateConstraintValidated(t, ctx, db, false)

			blocker, err := db.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = blocker.Rollback(ctx) }()
			if _, err := blocker.Exec(ctx, `SELECT id FROM project WHERE id = $1 FOR UPDATE`, id); err != nil {
				t.Fatalf("lock concurrent project: %v", err)
			}
			manager, err := home.NewWorkspaceManager(db, homeDir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = manager.Close() })
			done := make(chan error, 1)
			go func() { done <- reconcileProjectCoordinatesStrict(ctx, manager) }()
			select {
			case err := <-done:
				t.Fatalf("project coordinate migration completed before locked row changed: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			tc.concurrent(ctx, t, blocker, id)
			if err := blocker.Commit(ctx); err != nil {
				t.Fatalf("release project row lock: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatalf("complete project coordinate migration: %v", err)
			}
			assertProjectCoordinateConstraintValidated(t, ctx, db, true)

			var got string
			err = db.QueryRow(ctx, `SELECT base_dir FROM project WHERE id = $1`, id).Scan(&got)
			if !tc.wantRow {
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("read concurrently deleted project = %q, %v; want no rows", got, err)
				}
				return
			}
			if err != nil || got != tc.wantValue {
				t.Fatalf("concurrently canonicalized base_dir = %q, %v; want %q", got, err, tc.wantValue)
			}
		})
	}
}

func TestProjectBaseDirMigrationLeavesUnresolvablePhysicalPathFailClosed(t *testing.T) {
	db, provider := newTestDBAtMigration(t, projectBaseDirMigration-1)
	ctx := t.Context()

	userID := uuid.NewString()
	agentID := "a"
	seedProjectBaseDirOwners(t, ctx, db, userID, agentID)
	id := uuid.NewString()
	physicalSibling := "/srv/stella/users/" + userID + "/agents/" + agentID + "-sibling"
	if _, err := db.Exec(ctx, `
		INSERT INTO project (id, agent_id, user_id, name, base_dir)
		VALUES ($1, $2, $3, 'ambiguous-owner-prefix', $4)
	`, id, agentID, userID, physicalSibling); err != nil {
		t.Fatalf("seed ambiguous physical path: %v", err)
	}
	if _, err := provider.UpTo(ctx, projectBaseDirMigration); err != nil {
		t.Fatalf("install pending project coordinate migration: %v", err)
	}
	assertProjectCoordinateConstraintValidated(t, ctx, db, false)
	manager, err := home.NewWorkspaceManager(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if err := reconcileProjectCoordinatesStrict(ctx, manager); err == nil {
		t.Fatal("filesystem-aware finalizer accepted a path outside its durable owner root")
	}
	assertProjectCoordinateConstraintValidated(t, ctx, db, false)

	var got string
	if err := db.QueryRow(ctx, `SELECT base_dir FROM project WHERE id = $1`, id).Scan(&got); err != nil {
		t.Fatalf("read rejected project path: %v", err)
	}
	if got != physicalSibling {
		t.Fatalf("failed finalizer rewrote base_dir = %q, want %q", got, physicalSibling)
	}
	if _, err := db.Exec(ctx, `UPDATE project SET base_dir = '.' WHERE id = $1`, id); err != nil {
		t.Fatalf("repair project path: %v", err)
	}
	if err := reconcileProjectCoordinatesStrict(ctx, manager); err != nil {
		t.Fatalf("retry finalizer after repair: %v", err)
	}
	assertProjectCoordinateConstraintValidated(t, ctx, db, true)
}

func TestProjectBaseDirMigrationRejectsSpoofedOwnerPathOutsideConfiguredHome(t *testing.T) {
	for _, tc := range []struct {
		name string
		path func(userID, agentID string) string
	}{
		{
			name: "POSIX",
			path: func(userID, agentID string) string {
				return "/srv/unrelated/users/" + userID + "/agents/" + agentID + "/project"
			},
		},
		{
			name: "Windows",
			path: func(userID, agentID string) string {
				return `D:\unrelated\users\` + userID + `\agents\` + agentID + `\secret-project`
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, provider := newTestDBAtMigration(t, projectBaseDirMigration-1)
			ctx := t.Context()

			userID := uuid.NewString()
			agentID := "spoofed-project-coordinate-agent"
			seedProjectBaseDirOwners(t, ctx, db, userID, agentID)
			spoofed := tc.path(userID, agentID)
			if _, err := db.Exec(ctx, `
				INSERT INTO project (id, agent_id, user_id, name, base_dir)
				VALUES ($1, $2, $3, 'spoofed-owner-path', $4)
			`, uuid.NewString(), agentID, userID, spoofed); err != nil {
				t.Fatalf("seed spoofed physical path: %v", err)
			}
			if _, err := provider.UpTo(ctx, projectBaseDirMigration); err != nil {
				t.Fatalf("install pending project coordinate migration: %v", err)
			}
			manager, err := home.NewWorkspaceManager(db, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = manager.Close() })
			if err := reconcileProjectCoordinatesStrict(ctx, manager); err == nil {
				t.Fatal("filesystem-aware finalizer accepted a spoofed owner path outside configured Home")
			}
			assertProjectCoordinateConstraintValidated(t, ctx, db, false)
		})
	}
}

func seedProjectBaseDirOwners(t *testing.T, ctx context.Context, db *pgxpool.Pool, userID, agentID string) {
	t.Helper()
	if _, err := db.Exec(ctx, `INSERT INTO auth_user (id, email) VALUES ($1, $2)`, userID, userID+"@test.invalid"); err != nil {
		t.Fatalf("seed project owner: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO agent (id, name, workspace) VALUES ($1, $2, '')`, agentID, agentID); err != nil {
		t.Fatalf("seed project Agent: %v", err)
	}
}

func reconcileProjectCoordinatesStrict(ctx context.Context, manager *home.WorkspaceManager) error {
	result, err := manager.ReconcileProjectCoordinates(ctx)
	if err != nil {
		return err
	}
	if len(result.UnresolvedIDs) != 0 {
		return fmt.Errorf("%d project coordinates remain unresolved", len(result.UnresolvedIDs))
	}
	return nil
}

func assertProjectCoordinateConstraintValidated(t *testing.T, ctx context.Context, db *pgxpool.Pool, want bool) {
	t.Helper()
	var got bool
	if err := db.QueryRow(ctx, `
		SELECT convalidated
		FROM pg_constraint
		WHERE conrelid = 'project'::regclass
		  AND conname = 'project_base_dir_canonical_check'
	`).Scan(&got); err != nil {
		t.Fatalf("read project coordinate constraint: %v", err)
	}
	if got != want {
		t.Fatalf("project coordinate constraint validated = %v, want %v", got, want)
	}
}
