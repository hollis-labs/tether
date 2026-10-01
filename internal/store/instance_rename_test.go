package store

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestInstanceRename_PreservesAuthorityAcrossUpgradeAndRollback(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()
	// Apply the actual released schema before the additive rename, then
	// exercise the real migration against a populated non-default authority.
	before := fstest.MapFS{}
	entries, err := fs.ReadDir(embeddedMigrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "0034_" {
			continue
		}
		data, err := embeddedMigrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		before[entry.Name()] = &fstest.MapFile{Data: data}
	}
	if _, err := migrateFS(db, before); err != nil {
		t.Fatal(err)
	}
	const urn = "msg://agent/agent-mux/agt_rename0001"
	if _, err := db.Exec(`INSERT INTO registry_entries
        (urn, kind, mux_instance_id, display_name, created_at, updated_at)
        VALUES (?, 'agent', 'remote-authority', 'Kept', '2026-10-01', '2026-10-01')`, urn); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	check := func(want string) {
		t.Helper()
		var oldID, newID, gotURN string
		if err := db.QueryRow(`SELECT urn, mux_instance_id, tether_instance_id
            FROM registry_entries WHERE urn = ?`, urn).Scan(&gotURN, &oldID, &newID); err != nil {
			t.Fatal(err)
		}
		if gotURN != urn || oldID != want || newID != want {
			t.Fatalf("identity = %q, previous=%q, current=%q; want %q / %q", gotURN, oldID, newID, urn, want)
		}
	}
	check("remote-authority")
	// Current binary writes remain readable by the rollback binary.
	if _, err := db.Exec(`UPDATE registry_entries SET tether_instance_id = 'new-authority' WHERE urn = ?`, urn); err != nil {
		t.Fatal(err)
	}
	check("new-authority")
	// Rollback writes remain readable after returning to the new binary.
	if _, err := db.Exec(`UPDATE registry_entries SET mux_instance_id = 'rollback-authority' WHERE urn = ?`, urn); err != nil {
		t.Fatal(err)
	}
	check("rollback-authority")
	for _, column := range []string{"mux_instance_id", "tether_instance_id"} {
		id := urn + column
		if _, err := db.Exec(`INSERT INTO registry_entries
            (urn, kind, `+column+`, display_name, created_at, updated_at)
            VALUES (?, 'agent', 'insert-authority', 'Kept', '2026-10-01', '2026-10-01')`, id); err != nil {
			t.Fatal(err)
		}
		var previous, current string
		if err := db.QueryRow(`SELECT mux_instance_id, tether_instance_id FROM registry_entries WHERE urn = ?`, id).Scan(&previous, &current); err != nil {
			t.Fatal(err)
		}
		if previous != "insert-authority" || current != previous {
			t.Fatalf("%s insert: previous=%q, current=%q", column, previous, current)
		}
	}
}
