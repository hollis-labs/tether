package store

import (
	"fmt"
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestSessionCredentialsMigrationBackfillsBeforeUniqueIndex(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
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
	var current migration
	for _, m := range migrations {
		if m.version < 37 {
			before[fmt.Sprintf("%04d_%s.sql", m.version, m.name)] = &fstest.MapFile{Data: []byte(m.sql)}
		}
		if m.version == 37 {
			current = m
		}
	}
	if _, err := migrateFS(db, before); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, state string }{{"created", "created"}, {"terminal", "completed"}} {
		_, err := db.Exec(`INSERT INTO sessions(id,launch_id,project_id,logical_agent_id,provider_id,workspace,state,created_at,updated_at) VALUES (?,'launch','project','actor','provider','workspace',?,'2000-01-01T00:00:00Z','2000-01-01T00:00:00Z')`, row.id, row.state)
		if err != nil {
			t.Fatal(err)
		}
	}
	for i, row := range []struct{ id, kind, session string }{{"old", "session", "created"}, {"new", "session", "created"}, {"ended", "session", "terminal"}, {"orphan", "session", "missing"}, {"operator", "operator", ""}} {
		_, err := db.Exec(`INSERT INTO principals(principal_id,kind,display,token_hash,scopes_json,session_id,addresses_json,created_at) VALUES (?,?,?,?,'[]',?,'[]','2000-01-01T00:00:00Z')`, row.id, row.kind, row.id, fmt.Sprintf("%064x", i+1), row.session)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := migrateFS(db, fstest.MapFS{fmt.Sprintf("%04d_%s.sql", current.version, current.name): &fstest.MapFile{Data: []byte(current.sql)}}); err != nil {
		t.Fatal("backfill failed", err)
	}
	for _, tc := range []struct {
		id      string
		revoked bool
	}{{"old", true}, {"new", false}, {"ended", true}, {"orphan", true}, {"operator", false}} {
		var revoked bool
		if err := db.QueryRow(`SELECT revoked_at IS NOT NULL FROM principals WHERE principal_id=?`, tc.id).Scan(&revoked); err != nil {
			t.Fatal(err)
		}
		if revoked != tc.revoked {
			t.Fatal("wrong migrated revocation", tc.id, revoked)
		}
	}
	var expires string
	if err := db.QueryRow(`SELECT expires_at FROM principals WHERE principal_id='new'`).Scan(&expires); err != nil || expires == "" {
		t.Fatal("active session lacks expiry backstop", err)
	}
	if _, err := db.Exec(`INSERT INTO principals(principal_id,kind,display,token_hash,scopes_json,session_id,addresses_json,created_at) VALUES ('duplicate','session','duplicate',?,'[]','created','[]','2000-01-01T00:00:00Z')`, fmt.Sprintf("%064x", 99)); err == nil {
		t.Fatal("duplicate active session credential accepted")
	}
}
