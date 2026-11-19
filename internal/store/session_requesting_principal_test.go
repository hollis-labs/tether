package store

import (
	"database/sql"
	"fmt"
	"io/fs"
	"testing"
	"testing/fstest"
)

// An existing DB retains unknown historical creators; repeating startup does
// not repeat ALTER TABLE or fabricate provenance from a parent or credential.
func TestRequestingPrincipalMigration(t *testing.T) {
	db := openTempDB(t)
	defer func() { _ = db.Close() }()
	files, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := loadMigrations(files)
	if err != nil {
		t.Fatal(err)
	}
	before := fstest.MapFS{}
	for _, m := range migrations {
		if m.version < 109 {
			before[fmt.Sprintf("%04d_%s.sql", m.version, m.name)] = &fstest.MapFile{Data: []byte(m.sql)}
		}
	}
	if _, err := migrateFS(db, before); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions(id,launch_id,project_id,logical_agent_id,provider_id,workspace,state,created_at,updated_at) VALUES ('old','l','p','a','pv','w','completed','2000','2000')`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := migrateFS(db, files); err != nil {
			t.Fatal(err)
		}
	}
	var principal sql.NullString
	if err := db.QueryRow(`SELECT requesting_principal FROM sessions WHERE id='old'`).Scan(&principal); err != nil || principal.Valid {
		t.Fatalf("historical creator=%+v %v", principal, err)
	}
}
