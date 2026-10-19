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
	"github.com/spf13/cobra"
)

func TestWorkstreamProjectsCLI(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "projects.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const urn = "msg://agent/test/prj_aaaaaaaaaa"
	if _, err := db.DB().Exec(`INSERT INTO registry_entries(urn,kind,display_name,status,created_at,updated_at) VALUES(?,'project','test','active','now','now')`, urn); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.NewHandler(api.Deps{Workstreams: db, Service: struct{ api.LaunchService }{}}))
	defer srv.Close()
	oldFactory, oldJSON := registryClientFactory, workstreamJSON
	oldProjects, oldFilter := workstreamProjectURNs, workstreamProjectFilter
	oldName, oldWF, oldStatus, oldRef, oldCursor, oldLimit := workstreamName, workstreamWorkflowID, workstreamStatus, workstreamRef, workstreamCursor, workstreamLimit
	t.Cleanup(func() {
		registryClientFactory = oldFactory
		workstreamJSON = oldJSON
		workstreamProjectURNs = oldProjects
		workstreamProjectFilter = oldFilter
		workstreamName = oldName
		workstreamWorkflowID = oldWF
		workstreamStatus = oldStatus
		workstreamRef = oldRef
		workstreamCursor = oldCursor
		workstreamLimit = oldLimit
		workstreamUpdateCmd.Flags().Lookup("project-urn").Changed = false
	})
	registryClientFactory = func() (*client.Client, error) {
		return client.New("tcp:"+strings.TrimPrefix(srv.URL, "http://"), client.WithToken("")), nil
	}
	workstreamJSON = true
	workstreamProjectURNs = []string{urn, urn}
	workstreamName = "test"
	workstreamWorkflowID = "opaque"
	workstreamStatus = ""
	workstreamRef = ""
	workstreamCursor = ""
	workstreamLimit = 100
	run := func(cmd *cobra.Command, args ...string) string {
		t.Helper()
		var err error
		raw := captureStdout(t, func() { err = cmd.RunE(cmd, args) })
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	var created api.WorkstreamDTO
	if err := json.Unmarshal([]byte(run(workstreamCreateCmd)), &created); err != nil {
		t.Fatal(err)
	}
	if len(created.ProjectURNs) != 1 {
		t.Fatal(created)
	}
	workstreamProjectFilter = urn
	var page api.WorkstreamListResponse
	if err := json.Unmarshal([]byte(run(workstreamListCmd)), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Workstreams) != 1 {
		t.Fatal(page)
	}
	if err := workstreamUpdateCmd.Flags().Set("project-urn", ""); err != nil {
		t.Fatal(err)
	}
	var cleared api.WorkstreamDTO
	if err := json.Unmarshal([]byte(run(workstreamUpdateCmd, created.ID)), &cleared); err != nil {
		t.Fatal(err)
	}
	if len(cleared.ProjectURNs) != 0 {
		t.Fatal("CLI clear failed")
	}

	if err := db.CreateSession(store.SessionRow{ID: "s", State: "created"}, nil); err != nil {
		t.Fatal(err)
	}
	workstreamProjectURNs = []string{urn}
	var ensured api.WorkstreamDTO
	if err := json.Unmarshal([]byte(run(workstreamEnsureCmd, "s")), &ensured); err != nil {
		t.Fatal(err)
	}
	if len(ensured.ProjectURNs) != 1 {
		t.Fatal("CLI ensure lost seed")
	}
	workstreamProjectURNs = []string{}
	if err := json.Unmarshal([]byte(run(workstreamEnsureCmd, "s")), &ensured); err != nil {
		t.Fatal(err)
	}
	if len(ensured.ProjectURNs) != 1 {
		t.Fatal("CLI ensure rebound existing")
	}
	for _, cmd := range []*cobra.Command{workstreamCreateCmd, workstreamUpdateCmd, workstreamEnsureCmd, workstreamListCmd} {
		if cmd.Flags().Lookup("project-urn") == nil {
			t.Fatal("missing project flag")
		}
	}
}
