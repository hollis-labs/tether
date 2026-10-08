package main

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/store"
)

func TestWorkstreamUpdateCmd(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "ws.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	srv := httptest.NewServer(api.NewHandler(api.Deps{Workstreams: db}))
	t.Cleanup(srv.Close)
	ws, err := db.CreateWorkstream(store.WorkstreamRow{Name: "keep"})
	if err != nil {
		t.Fatal(err)
	}
	oldFactory, oldJSON := registryClientFactory, workstreamJSON
	t.Cleanup(func() { registryClientFactory, workstreamJSON = oldFactory, oldJSON })
	addr := "tcp:" + strings.TrimPrefix(srv.URL, "http://")
	registryClientFactory = func() (*client.Client, error) { return client.New(addr, client.WithToken("")), nil }
	workstreamJSON = true

	if err := workstreamUpdateCmd.Flags().Set("workflow-id", "torque:PRJ-1"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		workstreamUpdateCmd.Flags().Lookup("workflow-id").Changed = false
		workstreamWorkflowID = ""
	})
	var runErr error
	out := captureStdout(t, func() { runErr = workstreamUpdateCmd.RunE(workstreamUpdateCmd, []string{ws.ID}) })
	if runErr != nil {
		t.Fatalf("update: %v", runErr)
	}
	var got api.WorkstreamDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if got.WorkflowID != "torque:PRJ-1" || got.Name != "keep" {
		t.Errorf("got %+v", got)
	}
	if err := workstreamUpdateCmd.RunE(workstreamUpdateCmd, []string{"ghost"}); err == nil {
		t.Error("unknown id: want error")
	}
}
