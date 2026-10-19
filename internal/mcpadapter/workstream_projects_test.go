package mcpadapter

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/store"
)

func TestWorkstreamProjectsTools(t *testing.T) {
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
	// Ensure needs route registration only; unexpected lifecycle calls panic.
	svc := struct{ api.LaunchService }{}
	srv := httptest.NewServer(api.NewHandler(api.Deps{Workstreams: db, Service: svc}))
	defer srv.Close()
	a := &Adapter{token: "t", scopes: map[string]struct{}{"session.write": {}}, client: client.New("tcp:"+strings.TrimPrefix(srv.URL, "http://"), client.WithToken(""))}
	ctx := context.Background()
	raw, err := a.handleWorkstreamCreate(ctx, map[string]any{"project_urns": []any{urn, urn}})
	if err != nil {
		t.Fatal(err)
	}
	created := raw.(map[string]any)["workstream"].(api.WorkstreamDTO)
	if len(created.ProjectURNs) != 1 {
		t.Fatal(created)
	}
	raw, err = a.handleWorkstreamList(ctx, map[string]any{"project_urn": urn})
	if err != nil || len(raw.(map[string]any)["workstreams"].([]api.WorkstreamDTO)) != 1 {
		t.Fatalf("list: %+v %v", raw, err)
	}
	raw, err = a.handleWorkstreamUpdate(ctx, map[string]any{"id": created.ID, "name": "kept"})
	if err != nil || len(raw.(map[string]any)["workstream"].(api.WorkstreamDTO).ProjectURNs) != 1 {
		t.Fatalf("omitted: %+v %v", raw, err)
	}
	raw, err = a.handleWorkstreamUpdate(ctx, map[string]any{"id": created.ID, "project_urns": []any{}})
	if err != nil || len(raw.(map[string]any)["workstream"].(api.WorkstreamDTO).ProjectURNs) != 0 {
		t.Fatalf("clear: %+v %v", raw, err)
	}
	raw, err = a.handleWorkstreamEnsure(ctx, map[string]any{"session_id": "s", "project_urns": []string{urn}})
	if err != nil || len(raw.(map[string]any)["workstream"].(api.WorkstreamDTO).ProjectURNs) != 1 {
		t.Fatalf("ensure: %+v %v", raw, err)
	}

	raw, err = a.handleWorkstreamEnsure(ctx, map[string]any{"session_id": "s", "project_urns": []any{}})
	if err != nil || len(raw.(map[string]any)["workstream"].(api.WorkstreamDTO).ProjectURNs) != 1 {
		t.Fatalf("ensure rebound: %+v %v", raw, err)
	}
	for _, bad := range []any{nil, "bad", []int{1}} {
		if _, err := a.handleWorkstreamUpdate(ctx, map[string]any{"id": created.ID, "project_urns": bad}); err == nil {
			t.Fatalf("accepted invalid project array: %#v", bad)
		}
	}
}
