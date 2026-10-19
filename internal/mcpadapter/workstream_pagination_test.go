package mcpadapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
)

func TestWorkstreamListToolPagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("limit") != "2" || q.Get("cursor") != "opaque" {
			t.Errorf("query=%v", q)
		}
		if err := json.NewEncoder(w).Encode(api.WorkstreamListResponse{Workstreams: []api.WorkstreamDTO{{ID: "w"}}, NextCursor: "next", HasMore: true, Limit: 2}); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	a := &Adapter{client: client.New("tcp:"+strings.TrimPrefix(srv.URL, "http://"), client.WithToken(""))}
	for _, ref := range []bool{false, true} {
		args := map[string]any{"limit": float64(2), "cursor": "opaque"}
		if ref {
			args["ref"] = "task:x"
		}
		var out any
		var err error
		if ref {
			out, err = a.handleWorkstreamsForRef(context.Background(), args)
		} else {
			out, err = a.handleWorkstreamList(context.Background(), args)
		}
		if err != nil {
			t.Fatal(err)
		}
		got := out.(map[string]any)
		if got["has_more"] != true || got["complete"] != false || got["next_cursor"] != "next" {
			t.Fatalf("metadata: %+v", got)
		}
	}
	for _, bad := range []any{1.5, "bad", -1, nil} {
		if _, err := a.handleWorkstreamList(context.Background(), map[string]any{"limit": bad}); err == nil {
			t.Fatalf("bad limit accepted: %v", bad)
		}
	}
}
