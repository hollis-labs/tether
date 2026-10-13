package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	forumhost "github.com/hollis-labs/tether/internal/forums"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

type forumSessionLookup struct {
	api.LaunchService
	db *store.Store
}

func (s forumSessionLookup) GetSession(id string) (*store.SessionRow, error) {
	return s.db.GetSession(id)
}

func TestForumsThroughAuthenticatedDaemon(t *testing.T) {
	db, e := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	ids := identity.NewStore(db.DB())
	token, e := ids.Mint(context.Background(), identity.Principal{ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}})
	if e != nil {
		t.Fatal(e)
	}
	s := &Server{Config: Config{IdentityMode: identity.Observe}, Identity: ids, Forums: forumhost.New(db), Service: forumSessionLookup{db: db}}
	h := s.Handler()
	call := func(method, path, body, credential string, want int) map[string]any {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if credential != "" {
			r.Header.Set("Authorization", "Bearer "+credential)
		}
		r.Header.Set("X-Tether-Session-Id", "forged-session")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		var out map[string]any
		if e = json.Unmarshal(w.Body.Bytes(), &out); e != nil {
			t.Fatal(e)
		}
		return out
	}
	call("GET", "/forums", "", "", 403)
	call("POST", "/forums", `{"id":"portfolio","namespace":"app/torque/forum/portfolio","title":"Portfolio"}`, token, 201)
	r := call("POST", "/forums/portfolio/topics", `{"title":"Pilot"}`, token, 201)
	topic := r["topic"].(map[string]any)["id"].(string)
	call("POST", "/forums/topics/"+topic+"/posts", `{"body_md":"Attempt","provenance":{"id":"spoofed"}}`, token, 400)
	p := call("POST", "/forums/topics/"+topic+"/posts", `{"body_md":"Human post"}`, token, 201)["post"].(map[string]any)
	stamp := p["provenance"].(map[string]any)
	if stamp["id"] != identity.OperatorID || stamp["kind"] != "user" || stamp["verified"] != true {
		t.Fatalf("server stamp: %v", stamp)
	}
	if _, ok := stamp["session_id"]; ok {
		t.Fatal("forged header entered provenance")
	}
	call("GET", "/forums/topics/"+topic+"/posts?limit=abc", "", token, 400)
	call("GET", "/forums/topics/"+topic+"/posts?limit=101", "", token, 400)
	call("GET", "/forums/portfolio/search?query=Human", "", token, 200)
	call("GET", "/forums/posts/"+p["id"].(string)+"?detail=full", "", token, 200)
	// A session can own a forum registered by a host; its author/provenance must
	// derive from the resolved session row, never request metadata.
	if e = db.CreateSession(store.SessionRow{ID: "agent-session", State: "running", LaunchID: "launch", ProjectID: "project", WorkstreamID: sql.NullString{String: "workstream", Valid: true}}, nil); e != nil {
		t.Fatal(e)
	}
	agentToken, e := ids.Mint(context.Background(), identity.Principal{ID: "session-principal", Kind: "session", SessionID: "agent-session", Scopes: []string{"operate", "read"}})
	if e != nil {
		t.Fatal(e)
	}
	call("POST", "/forums", `{"id":"agent-owned","namespace":"app/test/forum/agent-owned","title":"Agent owned","owner_type":"agent","owner_id":"session-principal"}`, token, 201)
	call("POST", "/forums", `{"id":"forbidden","namespace":"app/test/forum/forbidden","title":"Denied"}`, agentToken, 403)
	call("GET", "/forums/portfolio", "", agentToken, 403)
	ar := call("POST", "/forums/agent-owned/topics", `{"title":"Agent topic"}`, agentToken, 201)
	atopic := ar["topic"].(map[string]any)["id"].(string)
	ap := call("POST", "/forums/topics/"+atopic+"/posts", `{"body_md":"Agent post"}`, agentToken, 201)["post"].(map[string]any)
	astamp := ap["provenance"].(map[string]any)
	if astamp["session_id"] != "agent-session" || astamp["project_id"] != "project" || astamp["launch_id"] != "launch" || astamp["verified"] != true {
		t.Fatalf("trusted session provenance missing: %v", astamp)
	}
	refs, e := db.ListSessionRefs("agent-session", store.ListSessionRefsOptions{Kind: store.KindForumPost})
	if e != nil || len(refs) != 1 {
		t.Fatalf("session refs: %v %v", refs, e)
	}
	if ap["provenance"].(map[string]any)["kind"] != "agent" {
		t.Fatal("agent stamped as user")
	}
	// Participant access is resolved from independent authenticated credentials.
	call("POST", "/forums/portfolio/participants", `{"principal_kind":"agent","principal_id":"session-principal","access":"post"}`, token, 201)
	call("POST", "/forums/portfolio/owner", `{}`, token, 201)
	if err := db.CreateSession(store.SessionRow{ID: "second-session", State: "running", LaunchID: "second-launch", ProjectID: "project", WorkstreamID: sql.NullString{String: "workstream", Valid: true}}, nil); err != nil {
		t.Fatal(err)
	}
	secondToken, err := ids.Mint(context.Background(), identity.Principal{ID: "second-principal", Kind: "session", SessionID: "second-session", Scopes: []string{"operate", "read"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(store.SessionRow{ID: "stranger-session", State: "running"}, nil); err != nil {
		t.Fatal(err)
	}
	strangerToken, err := ids.Mint(context.Background(), identity.Principal{ID: "uninvited-principal", Kind: "session", SessionID: "stranger-session", Scopes: []string{"operate", "read"}})
	if err != nil {
		t.Fatal(err)
	}
	call("POST", "/forums/portfolio/participants", `{"principal_kind":"agent","principal_id":"second-principal","access":"post"}`, token, 201)
	call("GET", "/forums/portfolio/participants", "", agentToken, 403)
	call("POST", "/forums/portfolio/participants/revoke", `{"principal_kind":"agent","principal_id":"second-principal"}`, agentToken, 403)
	call("POST", "/forums/portfolio/owner", `{}`, secondToken, 403)
	first := call("POST", "/forums/topics/"+topic+"/posts", `{"body_md":"PM participant"}`, agentToken, 201)["post"].(map[string]any)["id"].(string)
	replyBody, _ := json.Marshal(map[string]string{"body_md": "Second agent participant", "parent_post_id": first})
	reply := call("POST", "/forums/topics/"+topic+"/posts", string(replyBody), secondToken, 201)["post"].(map[string]any)
	if stamp := reply["provenance"].(map[string]any); stamp["id"] != "second-principal" || stamp["session_id"] != "second-session" || stamp["workstream_id"] != "workstream" {
		t.Fatal(stamp)
	}
	for _, credential := range []string{token, agentToken, secondToken} {
		call("GET", "/forums", "", credential, 200)
		call("GET", "/forums/portfolio", "", credential, 200)
		call("GET", "/forums/topics/"+topic, "", credential, 200)
		call("GET", "/forums/posts/"+first, "", credential, 200)
		call("GET", "/forums/portfolio/search?query=participant", "", credential, 200)
	}
	pg := call("GET", "/forums/topics/"+topic+"/posts?limit=1", "", secondToken, 200)
	cursor := pg["next_cursor"].(string)
	call("GET", "/forums/portfolio", "", strangerToken, 403)
	call("POST", "/forums/portfolio/participants/revoke", `{"principal_kind":"agent","principal_id":"second-principal"}`, token, 201)
	call("GET", "/forums/topics/"+topic+"/posts?limit=1&cursor="+cursor, "", secondToken, 403)
	call("GET", "/forums/topics/"+topic+"/posts?since_seq=1", "", secondToken, 403)
	call("POST", "/forums/topics/"+topic+"/posts", `{"body_md":"denied"}`, secondToken, 403)
	call("GET", "/forums/portfolio/search?query=participant", "", secondToken, 403)
	refs, err = db.ListSessionRefs("second-session", store.ListSessionRefsOptions{Kind: store.KindForumPost})
	if err != nil || len(refs) != 1 {
		t.Fatalf("invited refs: %v %v", refs, err)
	}
	for _, path := range []string{"/forums", "/forums/portfolio", "/forums/portfolio/topics", "/forums/portfolio/search", "/forums/topics/" + topic, "/forums/topics/" + topic + "/posts", "/forums/posts/" + p["id"].(string)} {
		if _, _, ok := api.RequiredLocalScope(httptest.NewRequest(http.MethodGet, path, nil)); !ok {
			t.Fatalf("unscoped forum route: %s", path)
		}
	}
}
