package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

func newDigestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "digest.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// Service is required even though no handler here uses it:
	// /sessions/{id}/digest is a sub-action of the /sessions/ route and
	// registerSessionRoutes returns early when Service is nil, so a server
	// with Digests and no Service serves no session digests at all. Same
	// latent coupling session_refs_test.go already documents for S2's
	// endpoint; the daemon always wires Service, so it is not live.
	srv := httptest.NewServer(NewHandler(Deps{
		Service: &fakeLaunchService{}, Workstreams: db, SessionRefs: db, Digests: db,
	}))
	t.Cleanup(srv.Close)
	return srv, db
}

func getDigest(t *testing.T, base, path string) (int, DigestResponse) {
	t.Helper()
	resp, err := http.Get(base + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out DigestResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}
	return resp.StatusCode, out
}

// TestDigestResponse_PopulatesEveryFieldItDeclares.
//
// Written in this shape because of the defect that closed S4: workstream_id was
// declared on the response and assigned on no path, and it survived review
// because every test checked the field it cared about. An always-empty SIBLING
// is invisible to a test that asserts one value. So this asserts the whole
// response, including the fields the case is not about.
func TestDigestResponse_PopulatesEveryFieldItDeclares(t *testing.T) {
	srv, db := newDigestServer(t)

	if err := db.CreateSession(store.SessionRow{ID: "s1", Intent: "fresh", State: "completed"}, &launch.Plan{}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := db.SetSessionRefAttribution("s1", store.RefAttributionNone); err != nil {
		t.Fatalf("SetSessionRefAttribution: %v", err)
	}
	ws, err := db.EnsureSessionWorkstream("s1", store.WorkstreamRow{Name: "w", WorkflowID: "wf-1"})
	if err != nil {
		t.Fatalf("EnsureSessionWorkstream: %v", err)
	}
	if _, err := db.AttachSessionRef(store.SessionRefRow{
		SessionID: "s1", Kind: store.KindTorqueTask, RefID: "CW-1",
		Relation: store.RelationCreated, Source: store.SourceAgent,
		At: "2026-09-12T01:00:00Z",
	}); err != nil {
		t.Fatalf("AttachSessionRef: %v", err)
	}

	code, d := getDigest(t, srv.URL, "/workstreams/"+ws.ID+"/digest")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	if d.Grain != "workstream" {
		t.Errorf("grain = %q, want workstream", d.Grain)
	}
	if d.Workstream == nil {
		t.Fatal("workstream is nil on a workstream-grain digest")
	}
	for name, got := range map[string]string{
		"workstream.id":          d.Workstream.ID,
		"workstream.name":        d.Workstream.Name,
		"workstream.workflow_id": d.Workstream.WorkflowID,
		"workstream.status":      d.Workstream.Status,
		"workstream.created_at":  d.Workstream.CreatedAt,
		"workstream.updated_at":  d.Workstream.UpdatedAt,
	} {
		if got == "" {
			t.Errorf("%s is empty", name)
		}
	}
	if d.Span.SessionCount != 1 || len(d.Span.Sessions) != 1 {
		t.Fatalf("span = %+v, want exactly one session", d.Span)
	}
	s := d.Span.Sessions[0]
	for name, got := range map[string]string{
		"session.id":              s.ID,
		"session.intent":          s.Intent,
		"session.state":           s.State,
		"session.created_at":      s.CreatedAt,
		"session.ref_attribution": s.RefAttribution,
	} {
		if got == "" {
			t.Errorf("%s is empty", name)
		}
	}
	if s.RefCount != 1 {
		t.Errorf("session.ref_count = %d, want 1", s.RefCount)
	}
	if len(d.LeftBehind) != 1 || len(d.LeftBehind[0].Created) != 1 {
		t.Fatalf("left_behind = %+v, want one created ref", d.LeftBehind)
	}
	ref := d.LeftBehind[0].Created[0]
	for name, got := range map[string]string{
		"ref.session_id": ref.SessionID,
		"ref.ref_id":     ref.RefID,
		"ref.relation":   ref.Relation,
		"ref.source":     ref.Source,
		"ref.at":         ref.At,
	} {
		if got == "" {
			t.Errorf("%s is empty", name)
		}
	}
	if d.Totals.Refs != 1 || d.Totals.ByRelation["created"] != 1 || d.Totals.BySource["agent"] != 1 {
		t.Errorf("totals = %+v, want one created/agent ref", d.Totals)
	}
	if d.Coverage.Limit == 0 {
		t.Error("coverage.limit is zero; the effective limit must always be reported")
	}
	if d.Coverage.Attribution[store.RefAttributionNone] != 1 {
		t.Errorf("coverage.attribution = %v, want one 'none'", d.Coverage.Attribution)
	}
	// An empty section must encode as [] and not null, so a consumer handles
	// one encoding of "nothing here" rather than two.
	if d.Touched == nil {
		t.Error("touched is null; an empty section must render as an empty array")
	}
}

// TestDigest_EmptyProxyColumnIsQualified. The digest must never let a reader
// take an absent proxy ref as evidence about what the agent did.
func TestDigest_EmptyProxyColumnIsQualified(t *testing.T) {
	srv, db := newDigestServer(t)
	if err := db.CreateSession(store.SessionRow{ID: "s1", Intent: "fresh", State: "completed"}, &launch.Plan{}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := db.SetSessionRefAttribution("s1", store.RefAttributionNone); err != nil {
		t.Fatalf("SetSessionRefAttribution: %v", err)
	}

	_, d := getDigest(t, srv.URL, "/sessions/s1/digest")
	if d.Coverage.ProxyAttributable != 0 {
		t.Fatalf("proxy_attributable = %d, want 0", d.Coverage.ProxyAttributable)
	}
	if d.Coverage.Note == "" {
		t.Fatal("coverage.note is empty while no session could produce a proxy ref: the emptiness must be explained, not merely shown")
	}
	if d.Workstream != nil {
		t.Error("workstream is set for a session that belongs to none")
	}
}

// TestWorkstreamsForRef_OverTheWire covers the encoding hazard specifically:
// a ref_id containing colons and slashes has to survive the query string and
// the first-colon split, or the lookup silently finds nothing.
func TestWorkstreamsForRef_OverTheWire(t *testing.T) {
	srv, db := newDigestServer(t)
	if err := db.CreateSession(store.SessionRow{ID: "s1", Intent: "fresh", State: "completed"}, &launch.Plan{}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	ws, err := db.EnsureSessionWorkstream("s1", store.WorkstreamRow{Name: "w"})
	if err != nil {
		t.Fatalf("EnsureSessionWorkstream: %v", err)
	}
	const urn = "msg://agent/agent-mux/agt_x9k2p4qrst"
	if _, err := db.AttachSessionRef(store.SessionRefRow{
		SessionID: "s1", Kind: "messaging_urn", RefID: urn,
		Relation: store.RelationReferenced, Source: store.SourceAgent,
		At: "2026-09-12T01:00:00Z",
	}); err != nil {
		t.Fatalf("AttachSessionRef: %v", err)
	}

	q := url.Values{}
	q.Set("ref", "messaging_urn:"+urn)
	resp, err := http.Get(srv.URL + "/workstreams?" + q.Encode())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out WorkstreamListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Workstreams) != 1 || out.Workstreams[0].ID != ws.ID {
		t.Fatalf("matched %+v, want the one workstream: a colon-bearing ref_id must survive the round trip", out.Workstreams)
	}
}

// TestWorkstreamsForRef_RejectsCombinedFilters. ref= and status= look like they
// compose and answer different questions; rejecting is cheaper than a
// confidently wrong result.
func TestWorkstreamsForRef_RejectsCombinedFilters(t *testing.T) {
	srv, _ := newDigestServer(t)
	resp, err := http.Get(srv.URL + "/workstreams?ref=torque_task:CW-1&status=active")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestDigest_NotFoundIsNotFound guards the error mapping: a missing session or
// workstream must 404 rather than 500, or a caller cannot tell "no such thing"
// from "we broke".
func TestDigest_NotFoundIsNotFound(t *testing.T) {
	srv, _ := newDigestServer(t)
	for _, path := range []string{"/sessions/nope/digest", "/workstreams/nope/digest"} {
		code, _ := getDigest(t, srv.URL, path)
		if code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
	}
}
