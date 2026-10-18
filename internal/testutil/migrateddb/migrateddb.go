// Package migrateddb gives tests a fully migrated database without replaying
// every migration. Test-only: nothing outside *_test.go files imports it.
package migrateddb

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

// Open returns a migrated database on disk, closed when the test ends. The
// caller's TestMain should call store.UseMigratedTemplate(); without it each
// call replays every migration, which costs ~4.5s under -race.
//
// Use it where the test needs the schema. A test of the migrations themselves
// should keep opening a blank database and calling store.Migrate.
func Open(t testing.TB) *sql.DB {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "migrated.db"))
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s.DB()
}
