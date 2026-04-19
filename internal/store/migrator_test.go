package store

import (
	"database/sql"
	"path/filepath"
	"sort"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite"
)

// openTempDB opens a fresh SQLite file in t.TempDir and returns the handle.
// The caller is responsible for closing it.
func openTempDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return db
}

// appliedVersions reads the schema_migrations table and returns the
// sorted list of recorded versions.
func appliedVersions(t *testing.T, db *sql.DB) []int {
	t.Helper()
	rows, err := db.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

// tableExists returns true if the named table exists in sqlite_master.
func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var got string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&got)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		t.Fatalf("sqlite_master lookup: %v", err)
	}
	return got == name
}

func TestMigrate_FreshDB_AppliesEmbedded(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	res, err := Migrate(db)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(res.Applied) == 0 || res.Applied[0] != 1 {
		t.Fatalf("expected Applied to contain version 1 first, got %+v", res)
	}
	if len(res.Stamped) != 0 {
		t.Fatalf("fresh DB should not stamp adopt; got %+v", res.Stamped)
	}
	// Every 0001 table must exist; new migrations in this embedded set can
	// add more, and those tests cover themselves.
	for _, tbl := range []string{"sessions", "launch_plans", "events", "schema_migrations"} {
		if !tableExists(t, db, tbl) {
			t.Fatalf("table %q missing after migrate", tbl)
		}
	}
	applied := appliedVersions(t, db)
	if len(applied) == 0 || applied[0] != 1 {
		t.Fatalf("schema_migrations rows = %v, want to begin with 1", applied)
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	first, err := Migrate(db)
	if err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	before := appliedVersions(t, db)

	res, err := Migrate(db)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(res.Applied) != 0 || len(res.Stamped) != 0 {
		t.Fatalf("second Migrate should be a no-op, got %+v (first: %+v)", res, first)
	}
	after := appliedVersions(t, db)
	if !intSliceEqual(before, after) {
		t.Fatalf("schema_migrations changed across idempotent Migrate calls: %v → %v", before, after)
	}
}

func TestMigrate_AdoptsExistingV001(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	// Pre-populate with v0.0.1 schema (same content as 0001_init.sql, but
	// representing a DB that was created before migrations existed).
	v001Schema := `
CREATE TABLE sessions (
    id TEXT PRIMARY KEY, launch_id TEXT NOT NULL, project_id TEXT NOT NULL,
    agent_id TEXT NOT NULL, provider_id TEXT NOT NULL, workspace TEXT NOT NULL,
    state TEXT NOT NULL, pid INTEGER, exit_code INTEGER,
    created_at TEXT NOT NULL, updated_at TEXT NOT NULL, ended_at TEXT);
CREATE TABLE launch_plans (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    plan_json TEXT NOT NULL);
CREATE TABLE events (
    id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL,
    at TEXT NOT NULL, kind TEXT NOT NULL, payload TEXT);`
	if _, err := db.Exec(v001Schema); err != nil {
		t.Fatalf("seed v0.0.1 schema: %v", err)
	}

	res, err := Migrate(db)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(res.Stamped) != 1 || res.Stamped[0] != 1 {
		t.Fatalf("expected version 1 stamped, got %+v", res)
	}
	// Any migrations above 0001 in the embedded set are applied normally;
	// only 0001 should be stamped (the v0.0.1 adoption path).
	for _, v := range res.Applied {
		if v == 1 {
			t.Fatalf("version 1 should be stamped, not applied: %+v", res)
		}
	}
	applied := appliedVersions(t, db)
	if len(applied) == 0 || applied[0] != 1 {
		t.Fatalf("schema_migrations rows = %v, want to begin with 1", applied)
	}
}

func TestMigrate_AppliesInOrderFromInjectedFS(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	fsys := fstest.MapFS{
		"0001_init.sql":  {Data: []byte(`CREATE TABLE t1 (id INTEGER);`)},
		"0003_third.sql": {Data: []byte(`CREATE TABLE t3 (id INTEGER);`)},
		"0002_second.sql": {Data: []byte(`CREATE TABLE t2 (id INTEGER);`)},
	}
	res, err := migrateFS(db, fsys)
	if err != nil {
		t.Fatalf("migrateFS: %v", err)
	}
	want := []int{1, 2, 3}
	sort.Ints(res.Applied)
	if !intSliceEqual(res.Applied, want) {
		t.Fatalf("Applied = %v, want %v", res.Applied, want)
	}
	for _, tbl := range []string{"t1", "t2", "t3"} {
		if !tableExists(t, db, tbl) {
			t.Fatalf("table %q missing after migrateFS", tbl)
		}
	}
	if got := appliedVersions(t, db); !intSliceEqual(got, want) {
		t.Fatalf("schema_migrations rows = %v, want %v", got, want)
	}
}

func TestMigrate_SkipsAlreadyApplied(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	fsys := fstest.MapFS{
		"0001_init.sql":   {Data: []byte(`CREATE TABLE t1 (id INTEGER);`)},
		"0002_second.sql": {Data: []byte(`CREATE TABLE t2 (id INTEGER);`)},
	}
	if _, err := migrateFS(db, fsys); err != nil {
		t.Fatalf("initial migrateFS: %v", err)
	}
	// Add a third migration and re-run; only the new one should apply.
	fsys["0003_third.sql"] = &fstest.MapFile{Data: []byte(`CREATE TABLE t3 (id INTEGER);`)}

	res, err := migrateFS(db, fsys)
	if err != nil {
		t.Fatalf("second migrateFS: %v", err)
	}
	if !intSliceEqual(res.Applied, []int{3}) {
		t.Fatalf("Applied = %v, want [3]", res.Applied)
	}
	if got := appliedVersions(t, db); !intSliceEqual(got, []int{1, 2, 3}) {
		t.Fatalf("schema_migrations rows = %v, want [1 2 3]", got)
	}
}

func TestMigrate_RejectsDuplicateVersion(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	fsys := fstest.MapFS{
		"0001_a.sql": {Data: []byte(`CREATE TABLE a (id INTEGER);`)},
		"0001_b.sql": {Data: []byte(`CREATE TABLE b (id INTEGER);`)},
	}
	if _, err := migrateFS(db, fsys); err == nil {
		t.Fatal("expected duplicate-version error, got nil")
	}
}

func TestMigrate_RejectsBadFilename(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	fsys := fstest.MapFS{
		"not-a-migration.sql": {Data: []byte(`CREATE TABLE x (id INTEGER);`)},
	}
	if _, err := migrateFS(db, fsys); err == nil {
		t.Fatal("expected filename-format error, got nil")
	}
}

func intSliceEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
