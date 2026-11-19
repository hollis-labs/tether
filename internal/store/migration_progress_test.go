package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
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
	if _, err := os.Stat(progress); !os.IsNotExist(err) {
		t.Fatalf("the migrating process left its progress file behind: %v", err)
	}
}

// #592 review: a process that opens the database while another is migrating it
// (a CLI beside the daemon) must not erase the migrating process's signal: only
// the process that wrote the file removes it.
func TestAnotherProcessesProgressFileIsKept(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fsys := fstest.MapFS{"0001_one.sql": {Data: []byte("CREATE TABLE one(x);")}}
	if _, err = migrateFS(db, fsys); err != nil {
		t.Fatal(err)
	}
	// The daemon (pid 1 here: always alive) is migrating.
	progress := MigrationProgressPath(dbPath)
	theirs := MigrationProgress{Version: 2, Name: "two", PID: 1}
	raw, _ := json.Marshal(theirs)
	if err = os.WriteFile(progress, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// This process opens the database with nothing to migrate.
	if _, err = migrateFSWithProgress(db, fsys, progress); err != nil {
		t.Fatal(err)
	}
	p, ok, err := ReadMigrationProgress(dbPath)
	if err != nil || !ok || p.Version != 2 || p.PID != 1 {
		t.Fatalf("the other process's progress was erased or changed: %+v ok=%v err=%v", p, ok, err)
	}
}

// A progress file whose process is gone (a daemon that died mid-migration) is
// stale: nothing is migrating.
func TestStaleProgressFileIsIgnored(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip("no `true` to get a dead pid:", err)
	}
	for pid, want := range map[int]bool{cmd.Process.Pid: false, os.Getpid(): true} {
		raw, _ := json.Marshal(MigrationProgress{Version: 9, Name: "nine", PID: pid})
		if err := os.WriteFile(MigrationProgressPath(dbPath), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := ReadMigrationProgress(dbPath); err != nil || ok != want {
			t.Errorf("pid %d: in progress %v, want %v (err %v)", pid, ok, want, err)
		}
	}
}
