package store

import (
	"fmt"
	"io/fs"
	"testing"
	"testing/fstest"
)

// Migration 0066 rebuilds session_idempotency with a per-principal primary
// key, keeps every row, and attributes daemon-derived team keys to the team
// runtime (CW-20261010-0263).
func TestSessionIdempotencyMigrationScopesKeysAndPreservesRows(t *testing.T) {
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
		name := fmt.Sprintf("%04d_%s.sql", m.version, m.name)
		switch {
		case m.version < 66:
			before[name] = &fstest.MapFile{Data: []byte(m.sql)}
		case m.version == 66:
			current = m
		}
	}
	if current.sql == "" {
		t.Fatal("migration 66 not found")
	}
	if _, err := migrateFS(db, before); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions(id,launch_id,project_id,logical_agent_id,provider_id,workspace,state,created_at,updated_at) VALUES ('s1','l','p','a','pr','w','created','2000-01-01T00:00:00Z','2000-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"hadron/run/1", "device_v1_abc", "team-port:nonce"} {
		if _, err := db.Exec(`INSERT INTO session_idempotency(key,operation,request_digest,session_id,resolved_parent_session_id) VALUES (?,'create','d','s1','parent')`, key); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := migrateFS(db, fstest.MapFS{fmt.Sprintf("%04d_%s.sql", current.version, current.name): &fstest.MapFile{Data: []byte(current.sql)}}); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"hadron/run/1": "legacy", "device_v1_abc": "legacy", "team-port:nonce": "team-runtime"} {
		var principal, digest, parent string
		if err := db.QueryRow(`SELECT principal_id,request_digest,resolved_parent_session_id FROM session_idempotency WHERE key=?`, key).Scan(&principal, &digest, &parent); err != nil {
			t.Fatalf("row %q lost: %v", key, err)
		}
		if principal != want || digest != "d" || parent != "parent" {
			t.Fatalf("row %q = %s/%s/%s, want principal %s with data intact", key, principal, digest, parent, want)
		}
	}
	insert := `INSERT INTO session_idempotency(principal_id,key,operation,request_digest,session_id) VALUES (?,?,'create','d','s1')`
	if _, err := db.Exec(insert, "msg://a", "hadron/run/1"); err != nil {
		t.Fatalf("a new principal could not reuse a legacy key: %v", err)
	}
	if _, err := db.Exec(insert, "msg://a", "hadron/run/1"); err == nil {
		t.Fatal("(principal, key) is not unique")
	}
	var idx int
	if err := db.QueryRow(`SELECT COUNT(1) FROM sqlite_master WHERE type='index' AND name='idx_session_idempotency_session'`).Scan(&idx); err != nil || idx != 1 {
		t.Fatal("session index lost", err)
	}
}
