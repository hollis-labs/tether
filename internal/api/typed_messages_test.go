package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/envelope"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/testutil/migrateddb"
)

type fixedCatalog struct{ cat *config.Catalog }

func (f fixedCatalog) Load() (*config.Catalog, error) { return f.cat, nil }

const (
	meURN     = "msg://agent/agent-mux/agt_me"
	archieURN = "msg://agent/agent-mux/agt_archie"
	bossURN   = "msg://agent/agent-mux/agt_boss"
)

type typedHarness struct {
	srv *httptest.Server
	db  *store.Store
}

func newTypedHarness(t *testing.T) *typedHarness {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "typed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := &config.Catalog{Agents: map[string]config.Agent{
		"photon-tether": {ID: "photon-tether", URN: meURN},
		"archie":        {ID: "archie", URN: archieURN},
		"boss":          {ID: "boss", URN: bossURN, Roles: []string{"manager"}},
	}}
	h := NewHandler(Deps{Service: &fakeLaunchService{}, MessageStore: db.MessagingStore(), Seats: db, Catalog: fixedCatalog{cat}})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &typedHarness{srv: srv, db: db}
}

func (h *typedHarness) do(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, bytes.NewReader([]byte(body)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func errorField(t *testing.T, raw []byte) string {
	t.Helper()
	var e ErrorResponse
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("decode error: %v (%s)", err, raw)
	}
	return e.Error.Details["field"]
}

func TestMessagePostRoundTrip(t *testing.T) {
	h := newTypedHarness(t)
	q := `{"kind":"question","to":["@archie"],"re":"task:CW-20261114-0013","subject":"Gate PR split?","body":"Split the Stop gate into its own PR?",
	 "options":[{"id":"a","label":"One PR"},{"id":"b","label":"Two PRs","recommended":true}],"default_option":"b"}`
	code, raw := h.do(t, http.MethodPost, "/messages/post?as="+meURN, q)
	if code != http.StatusCreated {
		t.Fatalf("post: %d %s", code, raw)
	}
	var out MessagePostResponse
	if err := json.Unmarshal(raw, &out); err != nil || len(out.IDs) != 1 || out.ThreadID == "" {
		t.Fatalf("post response: %v %s", err, raw)
	}
	if out.Envelope.From != "@photon-tether" || out.Envelope.Attention != envelope.AttentionAgent || out.Envelope.Schema != envelope.Schema {
		t.Fatalf("stamped envelope = %+v", out.Envelope)
	}
	stored, err := h.db.MessagingStore().Get(t.Context(), out.IDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if stored.ContentType != envelope.ContentType || string(stored.Kind) != "request" || stored.To.URN() != archieURN || stored.From.URN() != meURN {
		t.Fatalf("stored = %+v", stored)
	}

	// It now waits on @archie.
	code, raw = h.do(t, http.MethodGet, "/messages/pending-replies?as="+archieURN, "")
	var pending PendingRepliesResponse
	if code != http.StatusOK || json.Unmarshal(raw, &pending) != nil || len(pending.Pending) != 1 || pending.Pending[0].MessageID != out.IDs[0] || pending.Handle != "@archie" {
		t.Fatalf("pending: %d %s", code, raw)
	}

	// @archie answers; the reply joins the thread and the question stops pending.
	reply := `{"kind":"message","to":["@photon-tether"],"in_reply_to":"` + out.IDs[0] + `","body":"Two.","answer":{"option":"b"}}`
	code, raw = h.do(t, http.MethodPost, "/messages/post?as="+archieURN, reply)
	var rout MessagePostResponse
	if code != http.StatusCreated || json.Unmarshal(raw, &rout) != nil || rout.ThreadID != out.ThreadID {
		t.Fatalf("reply: %d %s (thread %q want %q)", code, raw, rout.ThreadID, out.ThreadID)
	}
	_, raw = h.do(t, http.MethodGet, "/messages/pending-replies?as="+archieURN, "")
	if json.Unmarshal(raw, &pending) != nil || len(pending.Pending) != 0 {
		t.Fatalf("still pending after the reply: %s", raw)
	}

	// An answer that is not one of the options is refused.
	bad := strings.Replace(reply, `"option":"b"`, `"option":"z"`, 1)
	if code, raw = h.do(t, http.MethodPost, "/messages/post?as="+archieURN, bad); code != http.StatusBadRequest || errorField(t, raw) != "answer.option" {
		t.Fatalf("bad answer: %d %s", code, raw)
	}
}

func TestMessagePostRejectsMalformedAndUnknownHandles(t *testing.T) {
	h := newTypedHarness(t)
	cases := map[string]struct{ body, field string }{
		"unknown handle":   {`{"kind":"message","to":["@unknown"],"body":"hi"}`, "to[0]"},
		"unknown awaiting": {`{"kind":"message","to":["@archie"],"awaiting":"@ghost","body":"hi"}`, "awaiting"},
		"bad kind":         {`{"kind":"chat","to":["@archie"],"body":"hi"}`, "kind"},
		"no body":          {`{"kind":"message","to":["@archie"]}`, "body"},
		"unknown field":    {`{"kind":"message","to":["@archie"],"body":"hi","prio":"high"}`, "(json)"},
		"from supplied":    {`{"kind":"message","from":"@archie","to":["@archie"],"body":"hi"}`, "from"},
		"question no opts": {`{"kind":"question","to":["@archie"],"body":"?"}`, "options"},
		"reply not mine":   {`{"kind":"message","to":["@archie"],"in_reply_to":"nope","body":"x"}`, "in_reply_to"},
	}
	for name, tc := range cases {
		code, raw := h.do(t, http.MethodPost, "/messages/post?as="+meURN, tc.body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, code, raw)
			continue
		}
		if got := errorField(t, raw); got != tc.field {
			t.Errorf("%s: field %q, want %q (%s)", name, got, tc.field, raw)
		}
	}
	if code, raw := h.do(t, http.MethodPost, "/messages/post", `{"kind":"message","to":["@archie"],"body":"hi"}`); code != http.StatusBadRequest {
		t.Fatalf("no sender accepted: %d %s", code, raw)
	}
}

func TestStatusSetAndCard(t *testing.T) {
	h := newTypedHarness(t)
	code, raw := h.do(t, http.MethodPost, "/status?as="+meURN, `{"state":"blocked","reason":"waiting on the libs tag","ref":"task:CW-20261114-0019"}`)
	if code != http.StatusBadRequest || errorField(t, raw) != "owner" {
		t.Fatalf("blocked without owner: %d %s", code, raw)
	}
	if code, raw = h.do(t, http.MethodPost, "/status?as="+meURN, `{"state":"blocked","owner":"@nobody"}`); code != http.StatusBadRequest || errorField(t, raw) != "owner" {
		t.Fatalf("unknown owner: %d %s", code, raw)
	}
	code, raw = h.do(t, http.MethodPost, "/status?as="+meURN, `{"state":"blocked","owner":"@archie","reason":"waiting on the libs tag","ttl_seconds":3600}`)
	var set StatusSetResponse
	if code != http.StatusOK || json.Unmarshal(raw, &set) != nil || !set.Changed || set.Handle != "@photon-tether" {
		t.Fatalf("set: %d %s", code, raw)
	}
	// Unchanged re-set: no change.
	_, raw = h.do(t, http.MethodPost, "/status?as="+meURN, `{"state":"blocked","owner":"@archie","reason":"waiting on the libs tag","ttl_seconds":3600}`)
	if json.Unmarshal(raw, &set) != nil || set.Changed {
		t.Fatalf("re-set changed: %s", raw)
	}
	// Another seat: refused for a peer, allowed for a manager.
	if code, _ = h.do(t, http.MethodPost, "/status?as="+archieURN, `{"target":"@photon-tether","state":"paused"}`); code != http.StatusForbidden {
		t.Fatalf("peer set another seat: %d", code)
	}
	if code, raw = h.do(t, http.MethodPost, "/status?as="+bossURN, `{"target":"@photon-tether","state":"working","reason":"unblocked"}`); code != http.StatusOK {
		t.Fatalf("manager set: %d %s", code, raw)
	}
	code, raw = h.do(t, http.MethodGet, "/status/@photon-tether", "")
	var card StatusCard
	if code != http.StatusOK || json.Unmarshal(raw, &card) != nil {
		t.Fatalf("card: %d %s", code, raw)
	}
	if card.Effective != "working" || card.Declared == nil || card.Declared.SetBy != bossURN || card.AgentURN != meURN {
		t.Fatalf("card = %s", raw)
	}
	if code, _ = h.do(t, http.MethodGet, "/status/@unknown", ""); code != http.StatusNotFound {
		t.Fatalf("unknown card: %d", code)
	}
}

func TestMessagePostUrgencyAndCatalogHandleOverride(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "typed2.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := &config.Catalog{Agents: map[string]config.Agent{
		"photon-tether": {ID: "photon-tether", URN: meURN},
		"cmd-builder-1": {ID: "cmd-builder-1", Handle: "builder-1", URN: "msg://agent/agent-mux/agt_b1"},
	}}
	srv := httptest.NewServer(NewHandler(Deps{Service: &fakeLaunchService{}, MessageStore: db.MessagingStore(), Seats: db, Catalog: fixedCatalog{cat}}))
	t.Cleanup(srv.Close)
	h := &typedHarness{srv: srv, db: db}
	body := `{"kind":"approval_request","to":["@builder-1"],"body":"Restart hub-tether?"}`
	if code, raw := h.do(t, http.MethodPost, "/messages/post?as="+meURN+"&urgency=critical", body); code != http.StatusBadRequest || errorField(t, raw) != "urgency" {
		t.Fatalf("bad urgency: %d %s", code, raw)
	}
	code, raw := h.do(t, http.MethodPost, "/messages/post?as="+meURN+"&urgency=high", body)
	var out MessagePostResponse
	if code != http.StatusCreated || json.Unmarshal(raw, &out) != nil {
		t.Fatalf("post: %d %s", code, raw)
	}
	stored, err := db.MessagingStore().Get(t.Context(), out.IDs[0])
	if err != nil || stored.Metadata["urgency"] != "high" || stored.To.URN() != "msg://agent/agent-mux/agt_b1" {
		t.Fatalf("stored = %+v err=%v", stored, err)
	}
	// The override is also how the seat is named back.
	code, raw = h.do(t, http.MethodPost, "/messages/post?as=msg://agent/agent-mux/agt_b1", `{"kind":"message","to":["@photon-tether"],"body":"ok"}`)
	if code != http.StatusCreated || json.Unmarshal(raw, &out) != nil || out.Envelope.From != "@builder-1" {
		t.Fatalf("from handle: %d %s", code, raw)
	}
}

func TestRegistryHandleSet(t *testing.T) {
	svc := registry.NewService(registry.NewStorage(migrateddb.Open(t)))
	srv := httptest.NewServer(NewHandler(Deps{Service: &fakeLaunchService{}, Registry: svc}))
	t.Cleanup(srv.Close)
	h := &typedHarness{srv: srv}
	ctx := t.Context()
	a, err := svc.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "Archie"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	if code, raw := h.do(t, http.MethodPost, "/registry/handles", `{"urn":"`+a.URN+`","handle":"@archie"}`); code != http.StatusOK {
		t.Fatalf("set: %d %s", code, raw)
	}
	if p, err := svc.LookupBy(ctx, registry.KindAgent, "archie", registry.HandleSubstrate); err != nil || p.URN != a.URN {
		t.Fatalf("lookup: %v %+v", err, p)
	}
	if code, raw := h.do(t, http.MethodPost, "/registry/handles", `{"urn":"`+b.URN+`","handle":"archie"}`); code != http.StatusConflict {
		t.Fatalf("taken handle: %d %s", code, raw)
	}
	for _, bad := range []string{"@Archie", "@chrispian", ""} {
		if code, _ := h.do(t, http.MethodPost, "/registry/handles", `{"urn":"`+b.URN+`","handle":"`+bad+`"}`); code != http.StatusBadRequest {
			t.Fatalf("handle %q accepted: %d", bad, code)
		}
	}
}

// The manager check follows identity, not handle: a manager with a handle:
// override may set others; a seat handed a manager's id as its handle may not.
func TestStatusSetManagerByIdentityNotHandle(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "typed3.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	reg := registry.NewService(registry.NewStorage(migrateddb.Open(t)))
	spoof, err := reg.Register(t.Context(), registry.KindAgent, registry.Profile{DisplayName: "Spoof"})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.AttachExternalID(t.Context(), spoof.URN, registry.HandleSubstrate, "boss"); err != nil {
		t.Fatal(err)
	}
	cat := &config.Catalog{Agents: map[string]config.Agent{
		"photon-tether": {ID: "photon-tether", URN: meURN},
		"boss":          {ID: "boss", Handle: "chief", URN: bossURN, Roles: []string{"manager"}},
	}}
	srv := httptest.NewServer(NewHandler(Deps{Service: &fakeLaunchService{}, MessageStore: db.MessagingStore(), Seats: db, Catalog: fixedCatalog{cat}, Registry: reg}))
	t.Cleanup(srv.Close)
	h := &typedHarness{srv: srv, db: db}
	if code, raw := h.do(t, http.MethodPost, "/status?as="+bossURN, `{"target":"@photon-tether","state":"paused"}`); code != http.StatusOK {
		t.Fatalf("manager with a handle override: %d %s", code, raw)
	}
	if code, raw := h.do(t, http.MethodPost, "/status?as="+spoof.URN, `{"target":"@photon-tether","state":"working"}`); code != http.StatusForbidden {
		t.Fatalf("seat holding a manager's id as its handle: %d %s", code, raw)
	}
}

func TestReservedOperatorHandleNeverNamesAnAgent(t *testing.T) {
	cat := &config.Catalog{Agents: map[string]config.Agent{"impostor": {ID: "impostor", Handle: "chrispian", URN: "msg://agent/agent-mux/agt_imp"}}}
	s := &Server{Catalog: fixedCatalog{cat}}
	if urn, ok := s.resolveHandle(t.Context(), "@chrispian"); !ok || urn != "msg://user/local/operator" {
		t.Fatalf("@chrispian -> %q %v", urn, ok)
	}
	if h := s.handleFor(t.Context(), "msg://agent/agent-mux/agt_imp"); h == "@chrispian" {
		t.Fatal("an agent is named @chrispian")
	}
}

// A worker session knows itself as msg://agent/agent-mux/<id> (TETHER_AGENT_URN) while mail is addressed to its
// catalog urn (agt_...): pending replies must match either form, and a reply from either settles.
func TestPendingRepliesMatchEveryAddressOfTheSeat(t *testing.T) {
	h := newTypedHarness(t)
	q := `{"kind":"question","to":["@photon-tether"],"body":"?","options":[{"id":"a","label":"A"},{"id":"b","label":"B"}]}`
	code, raw := h.do(t, http.MethodPost, "/messages/post?as="+archieURN, q)
	var out MessagePostResponse
	if code != http.StatusCreated || json.Unmarshal(raw, &out) != nil {
		t.Fatalf("post: %d %s", code, raw)
	}
	for _, as := range []string{meURN, "msg://agent/agent-mux/photon-tether"} {
		code, raw = h.do(t, http.MethodGet, "/messages/pending-replies?as="+as, "")
		var p PendingRepliesResponse
		if code != http.StatusOK || json.Unmarshal(raw, &p) != nil || len(p.Pending) != 1 {
			t.Fatalf("as %s: %d %s", as, code, raw)
		}
	}
	// Replying as the binding-target form settles it for both.
	reply := `{"kind":"message","to":["@archie"],"in_reply_to":"` + out.IDs[0] + `","body":"a","answer":{"option":"a"}}`
	if code, raw = h.do(t, http.MethodPost, "/messages/post?as=msg://agent/agent-mux/photon-tether", reply); code != http.StatusCreated {
		t.Fatalf("reply: %d %s", code, raw)
	}
	for _, as := range []string{meURN, "msg://agent/agent-mux/photon-tether"} {
		_, raw = h.do(t, http.MethodGet, "/messages/pending-replies?as="+as, "")
		var p PendingRepliesResponse
		if json.Unmarshal(raw, &p) != nil || len(p.Pending) != 0 {
			t.Fatalf("as %s after reply: %s", as, raw)
		}
	}
}

// A session credential lists its session address first; the post is stamped
// from the seat (the first address with a handle), not the session.
func TestMessagePostStampsTheSeatForASessionPrincipal(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "typed4.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := &config.Catalog{Agents: map[string]config.Agent{
		"photon-tether": {ID: "photon-tether", URN: meURN},
		"archie":        {ID: "archie", URN: archieURN},
	}}
	api := NewHandler(Deps{Service: &fakeLaunchService{}, MessageStore: db.MessagingStore(), Seats: db, Catalog: fixedCatalog{cat}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := identity.Principal{Verified: true, Kind: "session", ID: "msg://session/local/s1",
			Addresses: []string{"msg://session/local/s1", "msg://agent/agent-mux/photon-tether", meURN}, Scopes: []string{"*"}}
		api.ServeHTTP(w, r.WithContext(identity.WithPrincipal(r.Context(), p)))
	}))
	t.Cleanup(srv.Close)
	h := &typedHarness{srv: srv, db: db}
	code, raw := h.do(t, http.MethodPost, "/messages/post", `{"kind":"update","to":["@archie"],"body":"hi"}`)
	var out MessagePostResponse
	if code != http.StatusCreated || json.Unmarshal(raw, &out) != nil || out.Envelope.From != "@photon-tether" {
		t.Fatalf("post: %d %s", code, raw)
	}
	stored, err := db.MessagingStore().Get(t.Context(), out.IDs[0])
	if err != nil || stored.Metadata["from_verification"] != "verified" {
		t.Fatalf("stored = %+v err=%v", stored, err)
	}
	// The stored sender is the seat's catalog urn (where it receives), not the
	// agent-mux binding target it was listed under first.
	if stored.From.URN() != meURN {
		t.Fatalf("from = %s, want the catalog urn %s", stored.From.URN(), meURN)
	}
}

// A seat without a catalog urn: falls back to its agent-mux address.
func TestMessagePostStampsTheLogicalFormWithoutACatalogURN(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "typed5.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := &config.Catalog{Agents: map[string]config.Agent{
		"loner":  {ID: "loner"},
		"archie": {ID: "archie", URN: archieURN},
	}}
	api := NewHandler(Deps{Service: &fakeLaunchService{}, MessageStore: db.MessagingStore(), Seats: db, Catalog: fixedCatalog{cat}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := identity.Principal{Verified: true, Kind: "session", ID: "msg://session/local/s2",
			Addresses: []string{"msg://session/local/s2", "msg://agent/agent-mux/loner"}, Scopes: []string{"*"}}
		api.ServeHTTP(w, r.WithContext(identity.WithPrincipal(r.Context(), p)))
	}))
	t.Cleanup(srv.Close)
	h := &typedHarness{srv: srv, db: db}
	code, raw := h.do(t, http.MethodPost, "/messages/post", `{"kind":"update","to":["@archie"],"body":"hi"}`)
	var out MessagePostResponse
	if code != http.StatusCreated || json.Unmarshal(raw, &out) != nil || out.Envelope.From != "@loner" {
		t.Fatalf("post: %d %s", code, raw)
	}
	stored, err := db.MessagingStore().Get(t.Context(), out.IDs[0])
	if err != nil || stored.From.URN() != "msg://agent/agent-mux/loner" {
		t.Fatalf("from = %v err=%v", stored.From, err)
	}
}
