package store

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestForumHierarchyUpgradeRetainsContent(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()
	db.SetMaxOpenConns(1)
	sub, e := fs.Sub(embeddedMigrations, "migrations")
	if e != nil {
		t.Fatal(e)
	}
	prior := fstest.MapFS{}
	// Use the real previous migration set, then seed the old namespace-per-board schema.
	entries, e := fs.ReadDir(sub, ".")
	if e != nil {
		t.Fatal(e)
	}
	for _, entry := range entries {
		if entry.Name() < "0075_" {
			b, e := fs.ReadFile(sub, entry.Name())
			if e != nil {
				t.Fatal(e)
			}
			prior[entry.Name()] = &fstest.MapFile{Data: b}
		}
	}
	if _, e = migrateFS(db, prior); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(`PRAGMA foreign_keys=ON;
 INSERT INTO forums (id,namespace,title,owner_type,owner_id,created_at,updated_at,last_seq) VALUES ('old','app/test/forum/main','Old','user','operator','then','then',1);
 INSERT INTO forum_topics (id,forum_id,title,status,author_id,author_kind,created_at,last_activity_at,last_seq,post_count) VALUES ('thread','old','History','archived','author','agent','then','then',1,1);
 INSERT INTO forum_posts (id,forum_id,topic_id,seq,body_md,body_chars,author_id,author_kind,verified,created_at) VALUES ('post','old','thread',1,'retained',8,'author','agent',1,'then');
 INSERT INTO forum_participants (forum_id,principal_kind,principal_id,access) VALUES ('old','agent','reader','read');`); e != nil {
		t.Fatal(e)
	}
	if _, e = Migrate(db); e != nil {
		t.Fatal(e)
	}
	var body, principal, access, status string
	var archived bool
	if e = db.QueryRow(`SELECT p.body_md,p.author_id,t.status,t.archived,g.access FROM forum_posts p JOIN forum_topics t ON t.id=p.topic_id JOIN forum_participants g ON g.forum_id=p.forum_id WHERE p.id='post'`).Scan(&body, &principal, &status, &archived, &access); e != nil {
		t.Fatal(e)
	}
	if body != "retained" || principal != "author" || status != "open" || !archived || access != "read" {
		t.Fatalf("lost data: %q %q %q %v %q", body, principal, status, archived, access)
	}
	var post string
	if e = db.QueryRow(`SELECT post_id FROM forum_post_fts WHERE forum_post_fts MATCH 'retained'`).Scan(&post); e != nil || post != "post" {
		t.Fatalf("search: %s %v", post, e)
	}
	if _, e = db.Exec(`INSERT INTO forums (id,namespace,title,owner_type,owner_id,created_at,updated_at) VALUES ('second','app/test/forum/main','Second','user','operator','now','now')`); e != nil {
		t.Fatalf("namespace still unique: %v", e)
	}
	rows, e := db.Query(`PRAGMA foreign_key_check`)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign key corruption")
	}
	if e = rows.Err(); e != nil {
		t.Fatal(e)
	}
}
