package main

import (
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/store"
)

func migrationsCatalog(dbPath string) *config.Catalog {
	cat := &config.Catalog{}
	cat.Global.Catalog.Defaults.StateDB = dbPath
	return cat
}

// A live daemon holds the write lock; doctor must not report a failure.
func TestCheckMigrationsAgainstLockedLiveDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "tether.db")
	live, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Close() }()
	if _, err := live.DB().Exec(`BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = live.DB().Exec(`ROLLBACK`) }()

	res := checkMigrations(migrationsCatalog(dbPath))
	if res.Status != statusOK {
		t.Fatalf("status = %v (%s), want ok", res.Status, res.Message)
	}
}

func TestCheckMigrationsMissingDBDoesNotCreate(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "tether.db")
	res := checkMigrations(migrationsCatalog(dbPath))
	if res.Status == statusOK || res.Status == statusFail {
		t.Fatalf("status = %v, want warn", res.Status)
	}
}
