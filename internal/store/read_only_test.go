package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenReadOnlyDoesNotCreateOrMigrate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state #1?.db")
	if _, err := OpenReadOnly(path); err == nil {
		t.Fatal("opened a missing database")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("missing database was created: %v", err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var tables int
	if err := db.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table'").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatalf("reader initialized schema: %d tables", tables)
	}
	if _, err := db.DB().Exec("CREATE TABLE forbidden (id INTEGER)"); err == nil {
		t.Fatal("read-only handle allowed a write")
	}
}

func TestOpenReadOnlyWithWALWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.DB().Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = writer.DB().Exec("ROLLBACK") }()
	reader, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.ListMessages(10); err != nil {
		t.Fatalf("read while writer held: %v", err)
	}
	var timeout int
	if err := reader.DB().QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout <= 0 {
		t.Fatal("reader does not wait for brief lock contention")
	}
}
