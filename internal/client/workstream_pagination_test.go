package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
)

func TestWorkstreamClientPagination(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("workflow_id") != "wf" || r.URL.Query().Get("status") != "active" {
			t.Error("filters not preserved")
		}
		out := api.WorkstreamListResponse{Workstreams: []api.WorkstreamDTO{{ID: "last"}}, Complete: true, Limit: 100}
		if r.URL.Query().Get("cursor") == "" {
			out.Workstreams[0].ID = "first"
			out.HasMore = true
			out.Complete = false
			out.NextCursor = "opaque"
		} else if r.URL.Query().Get("cursor") != "opaque" {
			t.Error("cursor not preserved")
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	c := New("tcp:"+strings.TrimPrefix(srv.URL, "http://"), WithToken(""))
	page, err := c.ListWorkstreamsPage(context.Background(), WorkstreamListOptions{Status: "active", WorkflowID: "wf", Limit: 2})
	if err != nil || !page.HasMore || len(page.Workstreams) != 1 {
		t.Fatalf("page: %+v %v", page, err)
	}
	all, err := c.ListWorkstreams(context.Background(), "active", "wf")
	if err != nil || len(all) != 2 || calls != 3 {
		t.Fatalf("all: %+v calls=%d %v", all, calls, err)
	}
}

func TestWorkstreamClientRejectsIncompleteOrLoopingServer(t *testing.T) {
	for _, loop := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewEncoder(w).Encode(api.WorkstreamListResponse{Workstreams: []api.WorkstreamDTO{}, HasMore: loop, NextCursor: "same"}); err != nil {
				t.Error(err)
			}
		}))
		c := New("tcp:"+strings.TrimPrefix(srv.URL, "http://"), WithToken(""))
		if _, err := c.ListWorkstreams(context.Background(), "", ""); err == nil {
			t.Error("incomplete or looping server accepted")
		}
		srv.Close()
	}
}
