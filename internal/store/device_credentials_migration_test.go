package store

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func TestDeviceMigrationPreservesCredentialAndSessionAuthority(t *testing.T) {
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
		if m.version < 64 {
			before[fmt.Sprintf("%04d_%s.sql", m.version, m.name)] = &fstest.MapFile{Data: []byte(m.sql)}
		}
	}
	if _, err := migrateFS(db, before); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"active", "deleted", "source", "replacement"} {
		if _, err := db.Exec(`INSERT INTO sessions(id,launch_id,project_id,logical_agent_id,provider_id,workspace,state,created_at,updated_at) VALUES (?,'launch','project','actor','provider','workspace','created','2000-01-01T00:00:00Z','2000-01-01T00:00:00Z')`, id); err != nil {
			t.Fatal(err)
		}
	}
	insert := func(id, kind, session, hash string) error {
		_, err := db.Exec(`INSERT INTO principals(principal_id,kind,display,token_hash,scopes_json,session_id,addresses_json,created_at,expires_at) VALUES (?,?,?,?,'["read"]',?,'[]','2000-01-01T00:00:00Z','2099-01-01T00:00:00.000000000Z')`, id, kind, id, hash, session)
		return err
	}
	for _, r := range []struct{ id, kind, session, hash string }{{"operator", "operator", "", strings.Repeat("1", 64)}, {"active", "session", "active", strings.Repeat("2", 64)}, {"deleted", "session", "deleted", strings.Repeat("3", 64)}} {
		if err := insert(r.id, r.kind, r.session, r.hash); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO principals(id,principal_id,kind,display,token_hash,scopes_json,addresses_json,created_at) VALUES (99,'deleted-history','service','history',?,'[]','[]','now'); DELETE FROM principals WHERE id=99`, strings.Repeat("9", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(db); err != nil {
		t.Fatal("upgrade failed", err)
	}
	var hash string
	if err := db.QueryRow(`SELECT token_hash FROM principals WHERE principal_id='operator'`).Scan(&hash); err != nil || hash != strings.Repeat("1", 64) {
		t.Fatal("existing hash altered", err)
	}
	if err := insert("device", "device", "", strings.Repeat("4", 64)); err != nil {
		t.Fatal("device kind unavailable", err)
	}
	var deviceRowID int
	if err := db.QueryRow(`SELECT id FROM principals WHERE principal_id='device'`).Scan(&deviceRowID); err != nil || deviceRowID <= 99 {
		t.Fatal("credential row identity reused", err)
	}
	if err := insert("device", "device", "", strings.Repeat("5", 64)); err == nil {
		t.Fatal("duplicate device identity accepted")
	}
	if err := insert("duplicate", "session", "active", strings.Repeat("6", 64)); err == nil {
		t.Fatal("session uniqueness lost")
	}
	if _, err := db.Exec(`UPDATE sessions SET state='completed' WHERE id='active'; DELETE FROM sessions WHERE id='deleted'`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"active", "deleted"} {
		var revoked bool
		if err := db.QueryRow(`SELECT revoked_at IS NOT NULL FROM principals WHERE principal_id=?`, id).Scan(&revoked); err != nil || !revoked {
			t.Fatal("terminal/delete revocation lost", id, err)
		}
	}
	if err := insert("terminal", "session", "active", strings.Repeat("7", 64)); err == nil {
		t.Fatal("inactive session authority accepted")
	}
	if _, err := db.Exec(`INSERT INTO session_replacements(source_session_id,replacement_session_id,actor_uri,intent_key,binding_id,binding_generation,source_plan_digest,credential_scopes_json,credential_mode,committed_at) VALUES ('source','replacement','actor','intent','binding',1,'digest','[]','off','now')`); err != nil {
		t.Fatal(err)
	}
	if err := insert("replacement", "session", "replacement", strings.Repeat("8", 64)); err == nil {
		t.Fatal("retained replacement credential authority lost")
	}
}
