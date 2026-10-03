package store

import (
	"database/sql"
	"fmt"
	"io/fs"
	"reflect"
	"testing"
	"testing/fstest"
)

// Exercise the actual previous schema with representative durable legacy data;
// no production database is opened by this test.
func TestFabricMigrationPreservesLegacySchemaAndIdentity(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()
	files, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := loadMigrations(files)
	if err != nil {
		t.Fatal(err)
	}
	before := fstest.MapFS{}
	// Keep this migration fixture fixed; later additive migrations have their
	// own tests and must not change the legacy-schema comparison below.
	throughFabric := fstest.MapFS{}
	for _, m := range migrations {
		if m.version <= 48 {
			throughFabric[fmt.Sprintf("%04d_%s.sql", m.version, m.name)] = &fstest.MapFile{Data: []byte(m.sql)}
		}
		if m.version < 48 {
			before[fmt.Sprintf("%04d_%s.sql", m.version, m.name)] = &fstest.MapFile{Data: []byte(m.sql)}
		}
	}
	if _, err := migrateFS(db, before); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions(id,launch_id,project_id,logical_agent_id,provider_id,workspace,state,created_at,updated_at) VALUES ('legacy-session','launch','project','actor','provider','workspace','running','2000-01-01T00:00:00Z','2000-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO registry_entries(urn,kind,display_name,created_at,updated_at) VALUES ('msg://agent/example/preserved','agent','Legacy example','2000-01-01T00:00:00Z','2000-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	snapshot := legacySchema(t, db)
	sessionBefore := legacyRow(t, db, "sessions", "id", "legacy-session")
	registryBefore := legacyRow(t, db, "registry_entries", "urn", "msg://agent/example/preserved")
	result, err := migrateFS(db, throughFabric)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Applied, []int{48}) {
		t.Fatal(result)
	}
	if !reflect.DeepEqual(snapshot, legacySchema(t, db)) {
		t.Fatal("legacy schema changed")
	}
	if !reflect.DeepEqual(sessionBefore, legacyRow(t, db, "sessions", "id", "legacy-session")) {
		t.Fatal("legacy session changed")
	}
	if !reflect.DeepEqual(registryBefore, legacyRow(t, db, "registry_entries", "urn", "msg://agent/example/preserved")) {
		t.Fatal("legacy identity changed")
	}
	var foreignKeys int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 0 {
		t.Fatal("global FK policy changed", foreignKeys, err)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM fabric_actors").Scan(&count); err != nil || count != 0 {
		t.Fatal("migration performed implicit enrollment", count, err)
	}
	result, err = migrateFS(db, throughFabric)
	if err != nil || len(result.Applied) != 0 {
		t.Fatal("reopen is not idempotent", result, err)
	}
}
func legacySchema(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT name,coalesce(sql,'') FROM sqlite_master WHERE name NOT LIKE 'fabric_%' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := map[string]string{}
	for rows.Next() {
		var name, source string
		if err := rows.Scan(&name, &source); err != nil {
			t.Fatal(err)
		}
		result[name] = source
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
func legacyRow(t *testing.T, db *sql.DB, table, column, key string) []any {
	t.Helper()
	// Identifiers are test-owned constants; the row identity is parameterized.
	rows, err := db.Query("SELECT * FROM "+table+" WHERE "+column+"=?", key)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatal("missing legacy fixture")
	}
	result := make([]any, len(columns))
	targets := make([]any, len(columns))
	for i := range result {
		targets[i] = &result[i]
	}
	if err := rows.Scan(targets...); err != nil {
		t.Fatal(err)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
