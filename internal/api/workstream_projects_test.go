package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

func TestWorkstreamProjectsHTTP(t *testing.T) {
	srv, db := newDigestServer(t)
	const urn = "msg://agent/test/prj_aaaaaaaaaa"
	if _, err := db.DB().Exec(`INSERT INTO registry_entries(urn,kind,display_name,status,created_at,updated_at) VALUES(?,'project','test','active','now','now')`, urn); err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string, want int) WorkstreamDTO {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("%s %s status=%d want=%d", method, path, res.StatusCode, want)
		}
		var out WorkstreamDTO
		if want < 300 {
			if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	created := call("POST", "/workstreams", `{"project_urns":["`+urn+`"]}`, 201)
	if len(created.ProjectURNs) != 1 {
		t.Fatal(created)
	}
	kept := call("PATCH", "/workstreams/"+created.ID, `{"name":"renamed"}`, 200)
	if len(kept.ProjectURNs) != 1 {
		t.Fatal("omission cleared")
	}
	for _, bad := range []string{`{"project_urns":null}`, `{"project_urns":"bad"}`, `{"project_urns":["tether"]}`} {
		call("PATCH", "/workstreams/"+created.ID, bad, 400)
		call("POST", "/workstreams", bad, 400)
	}
	cleared := call("PATCH", "/workstreams/"+created.ID, `{"project_urns":[]}`, 200)
	if cleared.ProjectURNs == nil || len(cleared.ProjectURNs) != 0 {
		t.Fatalf("clear: %+v", cleared)
	}
	if err := db.CreateSession(store.SessionRow{ID: "s", State: "created"}, nil); err != nil {
		t.Fatal(err)
	}
	call("POST", "/sessions/s/workstream", `{"ensure":true,"project_urns":null}`, 400)
	seed := call("POST", "/sessions/s/workstream", `{"ensure":true,"project_urns":["`+urn+`"]}`, 200)
	again := call("POST", "/sessions/s/workstream", `{"ensure":true,"project_urns":[]}`, 200)
	if again.ID != seed.ID || len(again.ProjectURNs) != 1 {
		t.Fatal("ensure changed existing")
	}
}
