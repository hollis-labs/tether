package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/libs/mesh/core/messaging/delivery"
)

// CW-20261115-0046: reopening a store whose delivery schema is current must
// not rewrite messaging_delivery_schema.applied_at; a version change still
// applies the schema and records the new time.
func TestOpenWritesDeliverySchemaOnlyOnVersionChange(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	reopen := func() {
		t.Helper()
		s, err := openDB(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	row := func() (int, string) {
		t.Helper()
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var version int
		var applied string
		if err := db.QueryRow(`SELECT version, applied_at FROM messaging_delivery_schema WHERE component='delivery'`).Scan(&version, &applied); err != nil {
			t.Fatal(err)
		}
		return version, applied
	}
	setRow := func(version int, applied string) {
		t.Helper()
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`UPDATE messaging_delivery_schema SET version=?, applied_at=? WHERE component='delivery'`, version, applied); err != nil {
			t.Fatal(err)
		}
	}

	reopen()
	const sentinel = "2000-01-01T00:00:00Z"
	setRow(delivery.SQLiteSchemaVersion, sentinel)
	reopen()
	if version, applied := row(); version != delivery.SQLiteSchemaVersion || applied != sentinel {
		t.Fatalf("reopen at the current version rewrote the row: version %d applied_at %q", version, applied)
	}

	setRow(delivery.SQLiteSchemaVersion-1, sentinel)
	reopen()
	if version, applied := row(); version != delivery.SQLiteSchemaVersion || applied == sentinel {
		t.Fatalf("a version change was not applied: version %d applied_at %q", version, applied)
	}
}
