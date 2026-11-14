package migrations

import (
	"testing"

	"github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/core/store/migrate"
)

// TestOneSquashedVersion pins both sets to the squashed schema. A schema change after the port starts
// adds 00002 onwards; it never edits 00001, which migrated databases have already applied.
func TestOneSquashedVersion(t *testing.T) {
	fsys, root := FS()
	for _, d := range []store.Driver{store.DriverSQLite, store.DriverPostgres} {
		v, err := migrate.Source{FS: fsys, Root: root}.Versions(d)
		if err != nil {
			t.Fatal(err)
		}
		if len(v) == 0 || v[0] != 1 {
			t.Fatalf("%s versions = %v, want the set to start at 1 (00001_schema.sql)", d, v)
		}
	}
}

// TestMigrationParity keeps the dialect sets in step: every version exists in both.
func TestMigrationParity(t *testing.T) {
	fsys, root := FS()
	if err := migrate.CheckParity(migrate.Source{FS: fsys, Root: root}); err != nil {
		t.Fatal(err)
	}
}
