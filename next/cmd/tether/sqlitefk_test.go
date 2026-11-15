package main

import (
	"context"
	"testing"

	"github.com/hollis-labs/libs/core/store/storetest"
)

// TestBinaryKeepsForeignKeysOff: the binary imports internal/sqlitefk, so the database module's SQLite
// connections run with foreign keys off, as the old store did.
func TestBinaryKeepsForeignKeysOff(t *testing.T) {
	var on int
	if err := storetest.SQLite(t).Reader().QueryRowContext(context.Background(), `PRAGMA foreign_keys`).Scan(&on); err != nil {
		t.Fatal(err)
	}
	if on != 0 {
		t.Fatalf("foreign_keys = %d in the tether binary, want 0", on)
	}
}
