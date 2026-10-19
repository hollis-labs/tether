package store

import (
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"
)

const projectA = "msg://agent/test/prj_aaaaaaaaaa"
const projectB = "msg://agent/test/prj_bbbbbbbbbb"

func seedWorkstreamProject(t *testing.T, db *Store, urn, kind, status string) {
	t.Helper()
	if _, err := db.DB().Exec(`INSERT INTO registry_entries(urn,kind,display_name,status,created_at,updated_at) VALUES(?,?,?,?,?,?)`, urn, kind, "test", status, "2026-10-19T00:00:00Z", "2026-10-19T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
}

func TestWorkstreamProjects(t *testing.T) {
	db := openWorkstreamStore(t)
	seedWorkstreamProject(t, db, projectA, "project", "active")
	seedWorkstreamProject(t, db, projectB, "project", "active")
	old, err := db.CreateWorkstream(WorkstreamRow{ID: "old", WorkflowID: "opaque;parent=ws:unchanged"})
	if err != nil {
		t.Fatal(err)
	}
	if old.ProjectURNs == nil || len(old.ProjectURNs) != 0 {
		t.Fatalf("old association: %+v", old)
	}
	row, err := db.CreateWorkstream(WorkstreamRow{ID: "associated", ProjectURNs: []string{projectB, projectA, projectB}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(row.ProjectURNs, []string{projectA, projectB}) {
		t.Fatal(row.ProjectURNs)
	}
	if _, err := db.CreateWorkstream(WorkstreamRow{ID: "second", ProjectURNs: []string{projectA}}); err != nil {
		t.Fatal(err)
	}
	page, err := db.ListWorkstreamsPage(ListWorkstreamsOptions{ProjectURN: projectA, Limit: 1})
	if err != nil || !page.HasMore {
		t.Fatalf("filter: %+v %v", page, err)
	}
	if _, err := db.ListWorkstreamsPage(ListWorkstreamsOptions{ProjectURN: projectB, Cursor: page.NextCursor}); !errors.Is(err, ErrWorkstreamPage) {
		t.Fatalf("cross-project cursor: %v", err)
	}
	if _, err := db.DB().Exec(`UPDATE registry_entries SET status='deprecated' WHERE urn=?`, projectA); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetWorkstream(row.ID)
	if err != nil || !reflect.DeepEqual(got.ProjectURNs, row.ProjectURNs) {
		t.Fatalf("deprecated link lost: %+v %v", got, err)
	}
	same := []string{projectA, projectB}
	if _, err := db.UpdateWorkstream(row.ID, WorkstreamPatch{ProjectURNs: &same}); err != nil {
		t.Fatal("retaining deprecated", err)
	}
	if _, err := db.CreateWorkstream(WorkstreamRow{ID: "bad", ProjectURNs: []string{projectA}}); !errors.Is(err, ErrWorkstreamProject) {
		t.Fatalf("new deprecated link: %v", err)
	}
	if _, err := db.GetWorkstream("bad"); !errors.Is(err, ErrWorkstreamNotFound) {
		t.Fatal("failed create left row")
	}
	invalid := []string{"PRJ-20260418-0001"}
	name := "changed"
	if _, err := db.UpdateWorkstream(row.ID, WorkstreamPatch{Name: &name, ProjectURNs: &invalid}); !errors.Is(err, ErrWorkstreamProject) {
		t.Fatalf("invalid update: %v", err)
	}
	got, err = db.GetWorkstream(row.ID)
	if err != nil || got.Name != "" || len(got.ProjectURNs) != 2 {
		t.Fatalf("update not atomic: %+v %v", got, err)
	}
	cleared := []string{}
	got, err = db.UpdateWorkstream(row.ID, WorkstreamPatch{ProjectURNs: &cleared})
	if err != nil || got.ProjectURNs == nil || len(got.ProjectURNs) != 0 {
		t.Fatalf("clear: %+v %v", got, err)
	}
}

func TestWorkstreamProjectsEnsureAndRegistryFailure(t *testing.T) {
	db := openWorkstreamStore(t)
	seedWorkstreamProject(t, db, projectA, "project", "active")
	seedWorkstreamProject(t, db, projectB, "project", "active")
	if err := db.CreateSession(SessionRow{ID: "session", State: "created", ProjectID: "launch-context"}, nil); err != nil {
		t.Fatal(err)
	}
	first, err := db.EnsureSessionWorkstream("session", WorkstreamRow{ProjectURNs: []string{projectA}})
	if err != nil {
		t.Fatal(err)
	}
	same, err := db.EnsureSessionWorkstream("session", WorkstreamRow{ProjectURNs: []string{projectB}})
	if err != nil || same.ID != first.ID || !reflect.DeepEqual(same.ProjectURNs, []string{projectA}) {
		t.Fatalf("ensure rebound: %+v %v", same, err)
	}
	seedWorkstreamProject(t, db, "msg://agent/test/prj_wrongkind", "agent", "active")
	for _, invalid := range []string{"tether", "PRJ-20260418-0001", "msg://agent/test/agt_aaaaaaaaaa", "msg://agent/test/prj_missing", "msg://agent/test/prj_wrongkind"} {
		if _, err := db.CreateWorkstream(WorkstreamRow{ProjectURNs: []string{invalid}}); !errors.Is(err, ErrWorkstreamProject) {
			t.Fatalf("accepted %q: %v", invalid, err)
		}
	}
	if _, err := db.DB().Exec(`ALTER TABLE registry_entries RENAME TO registry_unavailable`); err != nil {
		t.Fatal(err)
	}
	name := "must rollback"
	projects := []string{projectB}
	if _, err := db.UpdateWorkstream(first.ID, WorkstreamPatch{Name: &name, ProjectURNs: &projects}); err == nil {
		t.Fatal("registry outage allowed write")
	}
	got, err := db.GetWorkstream(first.ID)
	if err != nil || got.Name != "" || !reflect.DeepEqual(got.ProjectURNs, []string{projectA}) {
		t.Fatalf("outage changed data/read: %+v %v", got, err)
	}
}

func TestWorkstreamProjectsMigrationPreservesLegacy(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	old := fstest.MapFS{}
	files, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	names, err := fs.ReadDir(files, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range names {
		if entry.Name() == "0090_workstream_projects.sql" {
			continue
		}
		raw, err := fs.ReadFile(files, entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		old[entry.Name()] = &fstest.MapFile{Data: raw}
	}
	if _, err := migrateFS(db, old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO workstreams(id,name,workflow_id,status,created_at,updated_at) VALUES('old','legacy','torque:EP-1;parent=ws:p','active','now','now')`); err != nil {
		t.Fatal(err)
	}
	result, err := Migrate(db)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Applied, []int{90}) {
		t.Fatalf("applied: %+v", result)
	}
	var correlation string
	var links int
	if err := db.QueryRow(`SELECT workflow_id FROM workstreams WHERE id='old'`).Scan(&correlation); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM workstream_projects`).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if correlation != "torque:EP-1;parent=ws:p" || links != 0 {
		t.Fatalf("implicit migration: %q links%d", correlation, links)
	}
}
