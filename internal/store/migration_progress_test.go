package store

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite"
)

// CW-20261115-0046: while a migration runs, its progress file names it and
// what remains; when migrating is over the file is gone.
func TestMigrationProgressFileTracksTheRunningMigration(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fsys := fstest.MapFS{
		"0001_one.sql":   {Data: []byte("CREATE TABLE one(x);")},
		"0002_two.sql":   {Data: []byte("CREATE TABLE two(x);")},
		"0003_three.sql": {Data: []byte("CREATE TABLE three(x);")},
	}
	var seen []MigrationProgress
	afterProgressWrite = func() {
		p, ok, err := ReadMigrationProgress(dbPath)
		if err != nil || !ok {
			t.Errorf("no progress while migrating: %v", err)
			return
		}
		seen = append(seen, p)
	}
	t.Cleanup(func() { afterProgressWrite = nil })
	progress := MigrationProgressPath(dbPath)
	if _, err = migrateFSWithProgress(db, fsys, progress); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 || seen[0].Version != 1 || seen[0].Name != "one" || !slices.Equal(seen[0].Remaining, []int{2, 3}) || seen[2].Version != 3 || len(seen[2].Remaining) != 0 || seen[1].PID == 0 {
		t.Fatalf("progress seen %+v", seen)
	}
	// MigrateWithProgress removes the file when it is done; migrateFSWithProgress leaves that to it.
	if _, err = MigrateWithProgress(db, progress); err == nil {
		if _, ok, _ := ReadMigrationProgress(dbPath); ok {
			t.Fatal("progress file left behind after migrating")
		}
	}
}
