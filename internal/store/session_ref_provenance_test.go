package store

import (
	"fmt"
	"io/fs"
	"reflect"
	"testing"
	"testing/fstest"
)

func TestSessionRefProvenanceMigrationPreservesClaimsAndIdentity(t *testing.T) {
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
	after := fstest.MapFS{}
	for _, m := range migrations {
		name := fmt.Sprintf("%04d_%s.sql", m.version, m.name)
		if m.version < 80 {
			before[name] = &fstest.MapFile{Data: []byte(m.sql)}
		}
		if m.version == 80 {
			after[name] = &fstest.MapFile{Data: []byte(m.sql)}
		}
	}
	if len(after) != 1 {
		t.Fatal("provenance migration missing")
	}
	if _, err := migrateFS(db, before); err != nil {
		t.Fatal(err)
	}
	for i, source := range []string{SourceProxy, SourceAPI, SourceAgent} {
		_, err = db.Exec(`INSERT INTO session_refs(id,session_id,kind,ref_id,uri,relation,source,at,parent_item_id) VALUES(?,'s','torque_task',?,'uri','read',?,'2000-01-01T00:00:00Z','parent')`, i+1, fmt.Sprint(i), source)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := migrateFS(db, after); err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db}
	rows, err := s.ListSessionRefs("s", ListSessionRefsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows=%v", rows)
	}
	for _, r := range rows {
		want := map[int64]string{1: SourceProxyClaimed, 2: SourceAPI, 3: SourceAgent}[r.ID]
		if r.Source != want || r.RefID != fmt.Sprint(r.ID-1) || r.URI != "uri" || r.Relation != RelationRead || r.At != "2000-01-01T00:00:00Z" || r.ParentItemID != "parent" {
			t.Fatalf("row changed: %+v", r)
		}
	}
	// The migration is recorded once, not replayed on open. All lookup indexes
	// must survive the table rebuild, and the idempotency constraint still holds.
	if _, err := migrateFS(db, after); err != nil {
		t.Fatal(err)
	}
	again, err := s.ListSessionRefs("s", ListSessionRefsOptions{})
	if err != nil || !reflect.DeepEqual(rows, again) {
		t.Fatal("repeated migration changed rows", err)
	}
	for _, name := range []string{"idx_session_refs_session", "idx_session_refs_lookup", "idx_session_refs_parent_item"} {
		var found string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&found); err != nil {
			t.Fatal("index missing", name, err)
		}
	}
	_, err = db.Exec(`INSERT INTO session_refs(session_id,kind,ref_id,relation,source,at) VALUES('s','torque_task','0','read','session','new')`)
	if err == nil {
		t.Fatal("unique reference identity lost")
	}
	for _, source := range []string{SourceSession, SourceOperator} {
		if _, err := s.AttachSessionRef(SessionRefRow{SessionID: "s", Kind: KindTorqueTask, RefID: source, Source: source}); err != nil {
			t.Fatal(err)
		}
	}
	// An untrusted historical claim can neither upgrade an assertion nor
	// overwrite a trusted proxy row through the store's terminal-source rule.
	for _, source := range []string{SourceAgent, SourceProxy} {
		ref := SessionRefRow{SessionID: "s2", Kind: KindTorqueTask, RefID: source, Source: source}
		if _, err := s.AttachSessionRef(ref); err != nil {
			t.Fatal(err)
		}
		ref.Source = SourceProxyClaimed
		result, err := s.AttachSessionRef(ref)
		if err != nil || result.Upgraded || result.Inserted {
			t.Fatalf("claimed row upgraded: %+v %v", result, err)
		}
		got, err := s.ListSessionRefs("s2", ListSessionRefsOptions{Source: source})
		if err != nil || len(got) != 1 {
			t.Fatalf("source overwritten: %v %v", got, err)
		}
	}
}
