package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

func TestWorkstreamPaginationHTTP(t *testing.T) {
	srv, db := newDigestServer(t)
	for _, id := range []string{"a", "b", "c"} {
		if _, err := db.CreateWorkstream(store.WorkstreamRow{ID: id, WorkflowID: "wf"}); err != nil {
			t.Fatal(err)
		}
	}
	get := func(path string, want int) WorkstreamListResponse {
		t.Helper()
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("%s: %d want %d", path, res.StatusCode, want)
		}
		var out WorkstreamListResponse
		if want == 200 {
			if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	first := get("/workstreams?limit=2&workflow_id=wf", 200)
	if first.Complete || !first.HasMore || first.NextCursor == "" || len(first.Workstreams) != 2 {
		t.Fatalf("first: %+v", first)
	}
	last := get("/workstreams?limit=1&workflow_id=wf&cursor="+url.QueryEscape(first.NextCursor), 200)
	if !last.Complete || last.HasMore || last.NextCursor != "" || len(last.Workstreams) != 1 {
		t.Fatalf("last: %+v", last)
	}
	get("/workstreams?workflow_id=other&cursor="+url.QueryEscape(first.NextCursor), 400)
	for _, suffix := range []string{"limit=-1", "limit=x", "limit=1.5", "cursor=garbage", "ref=bad", "ref=task:x&status=active"} {
		get("/workstreams?"+suffix, 400)
	}
	empty := get("/workstreams?workflow_id=absent", 200)
	if empty.Workstreams == nil || !empty.Complete {
		t.Fatalf("empty envelope: %+v", empty)
	}
}

func TestWorkstreamPaginationRef(t *testing.T) {
	srv, db := newDigestServer(t)
	const ref = "messaging_urn:msg://agent/test/a"
	for _, id := range []string{"a", "b", "c"} {
		if err := db.CreateSession(store.SessionRow{ID: id, State: "created"}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := db.EnsureSessionWorkstream(id, store.WorkstreamRow{ID: "w" + id}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AttachSessionRef(store.SessionRefRow{SessionID: id, Kind: "messaging_urn", RefID: "msg://agent/test/a", Relation: store.RelationReferenced, Source: store.SourceAgent, At: "2026-10-19T00:00:00Z"}); err != nil {
			t.Fatal(err)
		}
	}
	q := url.Values{"ref": {ref}, "limit": {"2"}}
	seen := 0
	for {
		res, err := http.Get(srv.URL + "/workstreams?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		var page WorkstreamListResponse
		err = json.NewDecoder(res.Body).Decode(&page)
		res.Body.Close()
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("ref page: %d %v", res.StatusCode, err)
		}
		seen += len(page.Workstreams)
		if page.Complete {
			break
		}
		wrong := url.Values{"ref": {"messaging_urn:other"}, "cursor": {page.NextCursor}}
		bad, err := http.Get(srv.URL + "/workstreams?" + wrong.Encode())
		if err != nil {
			t.Fatal(err)
		}
		bad.Body.Close()
		if bad.StatusCode != 400 {
			t.Fatal("cross-ref cursor accepted")
		}
		q.Set("cursor", page.NextCursor)
	}
	if seen != 3 {
		t.Fatalf("ref matches=%d", seen)
	}
}
