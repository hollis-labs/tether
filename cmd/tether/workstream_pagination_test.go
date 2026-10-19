package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
)

func TestWorkstreamListCmdPagination(t *testing.T) {
	oldFactory, oldJSON := registryClientFactory, workstreamJSON
	oldStatus, oldWF, oldRef, oldCursor, oldLimit := workstreamStatus, workstreamWorkflowID, workstreamRef, workstreamCursor, workstreamLimit
	t.Cleanup(func() {
		registryClientFactory = oldFactory
		workstreamJSON = oldJSON
		workstreamStatus = oldStatus
		workstreamWorkflowID = oldWF
		workstreamRef = oldRef
		workstreamCursor = oldCursor
		workstreamLimit = oldLimit
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("limit") != "2" || q.Get("cursor") != "opaque" || q.Get("workflow_id") != "wf" {
			t.Errorf("query: %v", q)
		}
		if err := json.NewEncoder(w).Encode(api.WorkstreamListResponse{Workstreams: []api.WorkstreamDTO{{ID: "w"}}, NextCursor: "next", HasMore: true, Limit: 2}); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	registryClientFactory = func() (*client.Client, error) {
		return client.New("tcp:"+strings.TrimPrefix(srv.URL, "http://"), client.WithToken("")), nil
	}
	workstreamJSON = true
	workstreamStatus = ""
	workstreamWorkflowID = "wf"
	workstreamRef = ""
	workstreamCursor = "opaque"
	workstreamLimit = 2
	var runErr error
	raw := captureStdout(t, func() { runErr = workstreamListCmd.RunE(workstreamListCmd, nil) })
	if runErr != nil {
		t.Fatal(runErr)
	}
	var out api.WorkstreamListResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	if !out.HasMore || out.Complete || out.NextCursor != "next" || len(out.Workstreams) != 1 {
		t.Fatalf("lost metadata: %+v", out)
	}
	for _, flag := range []string{"limit", "cursor", "ref"} {
		if workstreamListCmd.Flags().Lookup(flag) == nil {
			t.Fatal("missing flag", flag)
		}
	}
}
