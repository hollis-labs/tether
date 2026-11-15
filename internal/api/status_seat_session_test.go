package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

// #568 regression: a seat's own live session could not set its own status.
// Its principal is a session credential (msg://session/local/<id>) whose
// addresses do not include the seat: with ?as=<seat> the claim did not verify
// (403), and without it the status landed on the session URN. The session's
// own row, bound to the seat's logical agent, now makes it the seat; any other
// seat's session is still refused.
func TestSeatSessionSetsItsOwnStatus(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "status.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := &config.Catalog{Agents: map[string]config.Agent{
		"photon-tether": {ID: "photon-tether", URN: meURN},
		"archie":        {ID: "archie", URN: archieURN},
	}}
	svc := &fakeLaunchService{getRes: map[string]*store.SessionRow{
		"sess-me":     {ID: "sess-me", LogicalAgentID: "photon-tether", State: "running"},
		"sess-archie": {ID: "sess-archie", LogicalAgentID: "archie", State: "running"},
		"sess-ended":  {ID: "sess-ended", LogicalAgentID: "photon-tether", State: "completed"},
	}}
	h := NewHandler(Deps{Service: svc, MessageStore: db.MessagingStore(), Seats: db, Catalog: fixedCatalog{cat},
		IdentityMode: identity.Observe, Bus: events.NewBus(events.BusOptions{Persister: db})})
	// Real session credentials: verified by token, no seat in their addresses.
	principals := map[string]identity.Principal{
		"me":     {Verified: true, ID: "msg://session/local/sess-me", Kind: "session", SessionID: "sess-me"},
		"archie": {Verified: true, ID: "msg://session/local/sess-archie", Kind: "session", SessionID: "sess-archie"},
		"ended":  {Verified: true, ID: "msg://session/local/sess-ended", Kind: "session", SessionID: "sess-ended"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := principals[r.Header.Get("X-Test-Principal")]; ok {
			r = r.WithContext(identity.WithPrincipal(r.Context(), p))
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	set := func(principal, query, body string) (int, StatusSetResponse) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/status"+query, bytes.NewReader([]byte(body)))
		req.Header.Set("X-Test-Principal", principal)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var raw bytes.Buffer
		_, _ = raw.ReadFrom(resp.Body)
		var out StatusSetResponse
		_ = json.Unmarshal(raw.Bytes(), &out)
		if resp.StatusCode >= 300 {
			t.Logf("%s %s: %d %s", principal, query, resp.StatusCode, raw.String())
		}
		return resp.StatusCode, out
	}
	for name, c := range map[string]struct{ query, body string }{
		"without as":          {"", `{"state":"working","reason":"0130 fix"}`},
		"as the seat":         {"?as=" + meURN, `{"state":"working","reason":"0130 fix"}`},
		"as the logical form": {"?as=msg://agent/agent-mux/photon-tether", `{"state":"working","reason":"0130 fix"}`},
		"target @self":        {"", `{"state":"working","reason":"0130 fix","target":"@self"}`},
		"as the seat, @self":  {"?as=" + meURN, `{"state":"available","reason":"0130 fix","target":"@self"}`},
	} {
		code, out := set("me", c.query, c.body)
		if code != http.StatusOK || out.Declared.AgentURN != meURN || out.Declared.SetBy != meURN || out.Declared.SetByPrincipal != "msg://session/local/sess-me" {
			t.Errorf("%s: %d %+v, want the seat %s set by its own session", name, code, out.Declared, meURN)
		}
	}
	// Another seat's session may not set this seat's status, by as= or by target.
	if code, _ := set("archie", "?as="+meURN, `{"state":"blocked","reason":"x"}`); code != http.StatusForbidden {
		t.Fatalf("archie's session claiming photon-tether: %d, want 403", code)
	}
	if code, _ := set("archie", "", `{"state":"blocked","reason":"x","target":"@photon-tether"}`); code != http.StatusForbidden {
		t.Fatalf("archie's session targeting photon-tether: %d, want 403", code)
	}
	// An ended session is no longer the seat.
	if code, out := set("ended", "?as="+meURN, `{"state":"working","reason":"x"}`); code != http.StatusForbidden {
		t.Fatalf("an ended session claiming its seat: %d %+v, want 403", code, out)
	}
}
