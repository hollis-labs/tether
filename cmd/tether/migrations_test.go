package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func migrationsStatusOf(t *testing.T, dbPath string) MigrationsStatus {
	t.Helper()
	var out bytes.Buffer
	if err := writeMigrationsStatus(&out, dbPath, true); err != nil {
		t.Fatal(err)
	}
	var st MigrationsStatus
	if err := json.Unmarshal(out.Bytes(), &st); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	return st
}

// CW-20261115-0046: status reports what this binary would migrate, read-only;
// apply brings the database current.
func TestMigrationsStatusAndApply(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	if st := migrationsStatusOf(t, dbPath); st.Exists || len(st.Pending) != 0 {
		t.Fatalf("missing db: %+v", st)
	}
	latest := migrationCLIUpgradeFixture(t, dbPath)
	if st := migrationsStatusOf(t, dbPath); !st.Exists || !slices.Equal(st.Pending, []int{latest}) || st.InProgress != nil {
		t.Fatalf("one migration behind: %+v (want pending [%d])", st, latest)
	}
	migrationsApplyCmd.SetArgs(nil)
	if err := migrationsApplyCmd.Flags().Set("state-db", dbPath); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	migrationsApplyCmd.SetOut(&out)
	if err := migrationsApplyCmd.RunE(migrationsApplyCmd, nil); err != nil {
		t.Fatalf("apply: %v (%s)", err, out.String())
	}
	if st := migrationsStatusOf(t, dbPath); len(st.Pending) != 0 {
		t.Fatalf("after apply: %+v", st)
	}
}

// Build an actual pre-upgrade schema, leaving the latest migration unapplied.
// Deleting its marker from a current store leaves its schema changes behind,
// so a pending CREATE TABLE migration cannot run against that fixture.
func migrationCLIUpgradeFixture(t *testing.T, dbPath string) int {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "internal", "store", "migrations", "*.sql"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("migration fixture files: %v (%d found)", err, len(paths))
	}
	type migration struct {
		path, name string
		version    int
	}
	migrations := make([]migration, 0, len(paths))
	for _, path := range paths {
		version, name, _ := strings.Cut(strings.TrimSuffix(filepath.Base(path), ".sql"), "_")
		n, err := strconv.Atoi(version)
		if err != nil {
			t.Fatal(err)
		}
		migrations = append(migrations, migration{path: path, name: name, version: n})
	}
	slices.SortFunc(migrations, func(a, b migration) int { return a.version - b.version })
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
        version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL
    )`); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:len(migrations)-1] {
		raw, err := os.ReadFile(m.path)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(string(raw)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("fixture migration %d: %v", m.version, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`, m.version, m.name, "2026-01-01T00:00:00Z"); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return migrations[len(migrations)-1].version
}

// Host-only: migrations touch the local state database.
func TestMigrationsRefusedWithARemoteTarget(t *testing.T) {
	remoteTargetFixture(t, "tcp:127.0.0.1:8099")
	for _, path := range [][]string{{"migrations", "status"}, {"migrations", "apply"}} {
		cmd, _, err := rootCmd.Find(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := guardRemoteCommand(cmd, nil); err == nil {
			t.Fatalf("%v admitted with --target", path)
		}
	}
}
