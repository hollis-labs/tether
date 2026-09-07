package api

// session_bootstrap_test.go — T08 (messaging vNext, CW-20260906-0039)
// acceptance #2 fixture coverage for POST /sessions/bootstrap: standalone
// session-only, durable actor, missing provider ID, repeated hook calls,
// explicit publication, and reconnect. "private offline boot" is a
// client-side property (go-tether-client's bootstrap helper working with
// no reachable daemon) and is covered there, not here.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

func newSessionBootstrapServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "bootstrap.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv := httptest.NewServer(NewHandler(Deps{SessionBootstrap: db}))
	t.Cleanup(srv.Close)
	return srv, db
}

func postBootstrap(t *testing.T, base string, body map[string]any) (*http.Response, sessionBootstrapResponse) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(base+"/sessions/bootstrap", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	var out sessionBootstrapResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return resp, out
}

// TestSessionBootstrap_StandaloneSessionOnly is the "standalone
// session-only" fixture: no logical_agent_id, no explicit publication --
// a plain, ephemeral session identity.
func TestSessionBootstrap_StandaloneSessionOnly(t *testing.T) {
	srv, db := newSessionBootstrapServer(t)
	resp, out := postBootstrap(t, srv.URL, map[string]any{"session_id": "sess-standalone"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !out.Created || out.SessionID != "sess-standalone" {
		t.Fatalf("response = %+v, want created=true", out)
	}
	row, err := db.GetSession("sess-standalone")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if row.LogicalAgentID != "" {
		t.Errorf("logical_agent_id = %q, want empty for a standalone session", row.LogicalAgentID)
	}
	if row.Publication != "private-local" {
		t.Errorf("publication = %q, want private-local (default)", row.Publication)
	}
	if row.Intent != "preassigned" {
		t.Errorf("intent = %q, want preassigned (default)", row.Intent)
	}
	if row.State != "external" {
		t.Errorf("state = %q, want external", row.State)
	}
}

// TestSessionBootstrap_DurableActor is the "durable actor" fixture:
// logical_agent_id set, so this session bootstrap represents (or is
// associated with) a durable actor rather than a one-off session.
func TestSessionBootstrap_DurableActor(t *testing.T) {
	srv, db := newSessionBootstrapServer(t)
	_, out := postBootstrap(t, srv.URL, map[string]any{
		"session_id":       "sess-durable",
		"logical_agent_id": "agt_durable_worker",
	})
	if !out.Created {
		t.Fatalf("response = %+v, want created=true", out)
	}
	row, err := db.GetSession("sess-durable")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if row.LogicalAgentID != "agt_durable_worker" {
		t.Errorf("logical_agent_id = %q, want agt_durable_worker", row.LogicalAgentID)
	}
}

// TestSessionBootstrap_MissingProviderID is the "missing provider ID"
// fixture: no provider_mappings supplied at all -- must succeed (a
// launcher may not know the native provider session id yet, or ever).
func TestSessionBootstrap_MissingProviderID(t *testing.T) {
	srv, db := newSessionBootstrapServer(t)
	resp, out := postBootstrap(t, srv.URL, map[string]any{"session_id": "sess-no-provider"})
	if resp.StatusCode != http.StatusOK || !out.Created {
		t.Fatalf("status=%d out=%+v, want 200/created=true", resp.StatusCode, out)
	}
	mappings, err := db.ListSessionProviderMappings("sess-no-provider")
	if err != nil {
		t.Fatalf("list mappings: %v", err)
	}
	if len(mappings) != 0 {
		t.Errorf("mappings = %+v, want none", mappings)
	}
}

// TestSessionBootstrap_RepeatedHookCalls is the "repeated hook calls"
// fixture: calling bootstrap twice with the SAME preassigned session_id
// must not invent a competing identity -- the second call is a no-op on
// the row (created=false) and does not error.
func TestSessionBootstrap_RepeatedHookCalls(t *testing.T) {
	srv, db := newSessionBootstrapServer(t)
	_, first := postBootstrap(t, srv.URL, map[string]any{"session_id": "sess-repeated", "logical_agent_id": "agt_a"})
	if !first.Created {
		t.Fatalf("first call: %+v, want created=true", first)
	}
	resp, second := postBootstrap(t, srv.URL, map[string]any{"session_id": "sess-repeated", "logical_agent_id": "agt_a"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second call status = %d, want 200 (not an error)", resp.StatusCode)
	}
	if second.Created {
		t.Fatalf("second call: %+v, want created=false (no competing identity invented)", second)
	}
	row, err := db.GetSession("sess-repeated")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if row.LogicalAgentID != "agt_a" {
		t.Errorf("logical_agent_id = %q, want unchanged agt_a", row.LogicalAgentID)
	}
}

// TestSessionBootstrap_ExplicitPublication is the "explicit publication"
// fixture: the caller explicitly chooses published-local rather than
// accepting the private-local default.
func TestSessionBootstrap_ExplicitPublication(t *testing.T) {
	srv, db := newSessionBootstrapServer(t)
	_, out := postBootstrap(t, srv.URL, map[string]any{
		"session_id":  "sess-published",
		"publication": "published-local",
	})
	if !out.Created {
		t.Fatalf("response = %+v, want created=true", out)
	}
	row, err := db.GetSession("sess-published")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if row.Publication != "published-local" {
		t.Errorf("publication = %q, want published-local", row.Publication)
	}
}

// TestSessionBootstrap_Reconnect is the "reconnect" fixture: a session
// that already has a row (from an earlier bootstrap or a real launch)
// bootstraps again with a FRESH provider mapping -- the row itself is
// untouched (created=false) but the new mapping is applied, since
// UpsertSessionProviderMapping is itself idempotent per (session_id,
// owner, provider).
func TestSessionBootstrap_Reconnect(t *testing.T) {
	srv, db := newSessionBootstrapServer(t)
	if _, out := postBootstrap(t, srv.URL, map[string]any{"session_id": "sess-reconnect"}); !out.Created {
		t.Fatalf("initial bootstrap: %+v, want created=true", out)
	}

	resp, out := postBootstrap(t, srv.URL, map[string]any{
		"session_id": "sess-reconnect",
		"provider_mappings": []map[string]any{
			{"owner": "tether", "provider": "claude-code", "native_session_id": "native-abc123"},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reconnect status = %d, want 200", resp.StatusCode)
	}
	if out.Created {
		t.Fatalf("reconnect response = %+v, want created=false (existing session)", out)
	}
	mapping, err := db.GetSessionProviderMapping("sess-reconnect", "tether", "claude-code")
	if err != nil {
		t.Fatalf("get mapping: %v", err)
	}
	if !mapping.NativeSessionID.Valid || mapping.NativeSessionID.String != "native-abc123" {
		t.Errorf("native_session_id = %+v, want native-abc123", mapping.NativeSessionID)
	}
}

func TestSessionBootstrap_RequiresSessionID(t *testing.T) {
	srv, _ := newSessionBootstrapServer(t)
	resp, _ := postBootstrap(t, srv.URL, map[string]any{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestSessionBootstrap_ConcurrentCallsForSameSessionIDNeverFail is T11's
// independent security review evidence (CW-20260906-0042): GetSession
// and CreateSession have no transaction spanning them, so two genuinely
// concurrent bootstrap calls for the SAME session_id (the exact "second
// hook invocation" scenario this endpoint exists for, just racing
// instead of sequential) could both observe "not found" and both attempt
// CreateSession; the loser must not surface a raw 500 from the resulting
// constraint violation. Exactly one call must report Created=true.
func TestSessionBootstrap_ConcurrentCallsForSameSessionIDNeverFail(t *testing.T) {
	srv, _ := newSessionBootstrapServer(t)
	const sessionID = "sess-race-e2e"

	const racers = 8
	var wg sync.WaitGroup
	statuses := make([]int, racers)
	responses := make([]sessionBootstrapResponse, racers)
	wg.Add(racers)
	for i := range racers {
		go func(i int) {
			defer wg.Done()
			resp, out := postBootstrap(t, srv.URL, map[string]any{"session_id": sessionID, "intent": "preassigned"})
			statuses[i] = resp.StatusCode
			responses[i] = out
		}(i)
	}
	wg.Wait()

	var createdCount, okCount int
	for i, status := range statuses {
		if status != http.StatusOK {
			t.Errorf("racer %d: status = %d, want 200 (never a race-induced 500)", i, status)
			continue
		}
		okCount++
		if responses[i].Created {
			createdCount++
		}
		if responses[i].SessionID != sessionID {
			t.Errorf("racer %d: session_id = %q, want %q", i, responses[i].SessionID, sessionID)
		}
	}
	if okCount != racers {
		t.Fatalf("okCount = %d, want %d (every concurrent call must succeed)", okCount, racers)
	}
	if createdCount != 1 {
		t.Fatalf("createdCount = %d, want exactly 1", createdCount)
	}
}

func TestSessionBootstrap_NotConfigured_404(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Deps{}))
	t.Cleanup(srv.Close)
	resp, _ := postBootstrap(t, srv.URL, map[string]any{"session_id": "x"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
