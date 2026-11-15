package legacyguard

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func makeDB(t *testing.T, statements ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tether.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, s := range statements {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return p
}

// oldLayout is the shape of the old tree's database: its migrator table with applied versions and a
// domain table.
var oldLayout = []string{
	`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`,
	`INSERT INTO schema_migrations VALUES (101, '2026-11-14T00:00:00Z')`,
	`CREATE TABLE sessions (id TEXT PRIMARY KEY)`,
}

const markDDL = `CREATE TABLE tether_migration_mark (id INTEGER PRIMARY KEY CHECK (id = 1), state TEXT NOT NULL)`

func TestCheckAccepts(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "tether.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{
		"missing file":        filepath.Join(t.TempDir(), "tether.db"),
		"empty file":          empty,
		"new build (goose)":   makeDB(t, `CREATE TABLE goose_db_version (id INTEGER PRIMARY KEY, version_id INTEGER, is_applied INTEGER, tstamp TEXT)`),
		"completed migration": makeDB(t, append(append([]string{}, oldLayout...), markDDL, `INSERT INTO tether_migration_mark VALUES (1, 'complete')`)...),
	} {
		if err := Check(p); err != nil {
			t.Errorf("%s: Check = %v, want nil", name, err)
		}
	}
}

func TestCheckRefusesOldLayout(t *testing.T) {
	p := makeDB(t, oldLayout...)
	before, _ := os.ReadFile(p)
	if err := Check(p); !errors.Is(err, ErrLegacyLayout) {
		t.Fatalf("Check(old layout) = %v, want ErrLegacyLayout", err)
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("Check modified the old database")
	}
}

func TestCheckRefusesUnfinishedMigration(t *testing.T) {
	for name, stmts := range map[string][]string{
		"running":    append(append([]string{}, oldLayout...), markDDL, `INSERT INTO tether_migration_mark VALUES (1, 'running')`),
		"no row yet": append(append([]string{}, oldLayout...), markDDL),
	} {
		if err := Check(makeDB(t, stmts...)); !errors.Is(err, ErrIncompleteMigration) {
			t.Errorf("%s: Check = %v, want ErrIncompleteMigration", name, err)
		}
	}
}

// TestCheckMigrationTarget: migrate-from opens a database whose migration is in progress (it finishes it),
// but never an old-layout database (that is a source, never a target).
func TestCheckMigrationTarget(t *testing.T) {
	for name, stmts := range map[string][]string{
		"running":    append(append([]string{}, oldLayout...), markDDL, `INSERT INTO tether_migration_mark VALUES (1, 'running')`),
		"bulk":       append(append([]string{}, oldLayout...), markDDL, `INSERT INTO tether_migration_mark VALUES (1, 'bulk')`),
		"no row yet": append(append([]string{}, oldLayout...), markDDL),
	} {
		if err := CheckMigrationTarget(makeDB(t, stmts...)); err != nil {
			t.Errorf("%s: CheckMigrationTarget = %v, want nil", name, err)
		}
	}
	if err := CheckMigrationTarget(makeDB(t, oldLayout...)); !errors.Is(err, ErrLegacyLayout) {
		t.Errorf("old layout: %v, want ErrLegacyLayout", err)
	}
}
