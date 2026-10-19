package client

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/store"
)

func TestWorkstreamProjectsClient(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "projects.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const urn = "msg://agent/test/prj_aaaaaaaaaa"
	if _, err := db.DB().Exec(`INSERT INTO registry_entries(urn,kind,display_name,status,created_at,updated_at) VALUES(?,'project','test','active','now','now')`, urn); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(store.SessionRow{ID: "s", State: "created"}, nil); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.NewHandler(api.Deps{Workstreams: db, Service: &funcService{}}))
	defer srv.Close()
	c := New("tcp:"+strings.TrimPrefix(srv.URL, "http://"), WithToken(""))
	ctx := context.Background()
	created, err := c.CreateWorkstream(ctx, "test", "opaque", urn, urn)
	if err != nil || len(created.ProjectURNs) != 1 {
		t.Fatalf("create: %+v %v", created, err)
	}
	page, err := c.ListWorkstreamsPage(ctx, WorkstreamListOptions{ProjectURN: urn})
	if err != nil || len(page.Workstreams) != 1 {
		t.Fatalf("filter: %+v %v", page, err)
	}
	name := "updated"
	kept, err := c.UpdateWorkstream(ctx, created.ID, api.WorkstreamUpdateRequest{Name: &name})
	if err != nil || len(kept.ProjectURNs) != 1 {
		t.Fatalf("omission: %+v %v", kept, err)
	}
	empty := []string{}
	cleared, err := c.UpdateWorkstream(ctx, created.ID, api.WorkstreamUpdateRequest{ProjectURNs: &empty})
	if err != nil || len(cleared.ProjectURNs) != 0 {
		t.Fatalf("clear: %+v %v", cleared, err)
	}
	seed, err := c.EnsureSessionWorkstream(ctx, "s", "seed", "", urn)
	if err != nil || len(seed.ProjectURNs) != 1 {
		t.Fatalf("ensure: %+v %v", seed, err)
	}
	again, err := c.EnsureSessionWorkstream(ctx, "s", "ignored", "")
	if err != nil || again.ID != seed.ID || len(again.ProjectURNs) != 1 {
		t.Fatalf("ensure rebound: %+v %v", again, err)
	}
}
