package migrations

import (
	"testing"

	"github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/core/store/migrate"
)

// TestOneSquashedVersion pins the set to the squashed schema. A schema change after the port starts
// adds 00002 onwards; it never edits 00001, which migrated databases have already applied.
func TestOneSquashedVersion(t *testing.T) {
	fsys, root := FS()
	v, err := migrate.Source{FS: fsys, Root: root}.Versions(store.DriverSQLite)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) == 0 || v[0] != 1 {
		t.Fatalf("versions = %v, want the set to start at 1 (00001_schema.sql)", v)
	}
}
