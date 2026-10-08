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

func TestWorkstreamUpdateTool(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "ws.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	srv := httptest.NewServer(api.NewHandler(api.Deps{Workstreams: db}))
	t.Cleanup(srv.Close)
	ws, err := db.CreateWorkstream(store.WorkstreamRow{Name: "keep", WorkflowID: "wf-0"})
	if err != nil {
		t.Fatal(err)
	}
	a := &Adapter{
		token:  "t",
		scopes: map[string]struct{}{"session.write": {}},
		client: client.New("tcp:"+strings.TrimPrefix(srv.URL, "http://"), client.WithToken("")),
	}

	out, err := a.handleWorkstreamUpdate(context.Background(), map[string]any{"id": ws.ID, "workflow_id": "torque:PRJ-1"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	got := out.(map[string]any)["workstream"].(api.WorkstreamDTO)
	if got.WorkflowID != "torque:PRJ-1" || got.Name != "keep" {
		t.Errorf("got %+v, want workflow changed and name kept", got)
	}

	if _, err := a.handleWorkstreamUpdate(context.Background(), map[string]any{"id": "ghost", "name": "x"}); err == nil {
		t.Error("unknown id: want error")
	}
	a.scopes = map[string]struct{}{}
	if _, err := a.handleWorkstreamUpdate(context.Background(), map[string]any{"id": ws.ID, "name": "x"}); err == nil {
		t.Error("missing scope: want error")
	}
}
