package daemon

// whoami_route_test.go — T08 (messaging vNext, CW-20260906-0039)
// regression test: internal/api.NewHandler correctly mounted /whoami and
// POST /sessions/bootstrap internally, but this package's Handler()
// wraps that in its OWN explicit path allowlist (a defense against an
// accidental catch-all "/" shadowing /health) -- forgetting to add a new
// route here means it's reachable in every internal/api-level test (they
// call api.NewHandler directly) but 404s through the actual production
// daemon, which always goes through THIS package's Handler(). This test
// exercises the real daemon.Server.Handler() path, not api.NewHandler
// directly, so it would have caught that exact class of gap.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/delivery"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

// stubCatalogLoader trivially satisfies api.CatalogLoader so Handler()'s
// outer "mount the api routes at all" gate is satisfied without needing
// a full LaunchService stub -- this test is about the Registry/
// SessionBootstrap-gated route allowlist, not session launch.
type stubCatalogLoader struct{}

func (stubCatalogLoader) Load() (*config.Catalog, error) { return &config.Catalog{}, nil }

func newWhoamiRouteTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := registry.NewService(registry.NewStorage(db))

	st, err := store.Open(t.TempDir() + "/whoami-route.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := &Server{
		Catalog:          stubCatalogLoader{},
		Registry:         svc,
		Groups:           svc,
		SessionBootstrap: st,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestDaemonHandler_WhoamiRouteReachable(t *testing.T) {
	ts := newWhoamiRouteTestServer(t)
	resp, err := http.Get(ts.URL + "/whoami?as=msg://session/agent-mux/sess_x")
	if err != nil {
		t.Fatalf("GET /whoami: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (route must be mounted through the real daemon.Server.Handler(), not just api.NewHandler)", resp.StatusCode)
	}
}

func TestDaemonHandler_SessionBootstrapRouteReachable(t *testing.T) {
	ts := newWhoamiRouteTestServer(t)
	body := strings.NewReader(`{"session_id":"sess-route-test"}`)
	resp, err := http.Post(ts.URL+"/sessions/bootstrap", "application/json", body)
	if err != nil {
		t.Fatalf("POST /sessions/bootstrap: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		SessionID string `json:"session_id"`
		Created   bool   `json:"created"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.SessionID != "sess-route-test" || !out.Created {
		t.Fatalf("bootstrap response = %+v, want created=true for a first call", out)
	}
}

// TestDaemonHandler_RetentionCandidatesRouteReachable is T09's instance of
// this same regression class: GET /messages/retention/candidates is
// served through the already-mounted /messages/ subtree inside
// api.NewHandler (gated on Server.Retention there), but this package's
// Handler() gates the WHOLE /messages/ subtree mount on s.MessageStore,
// not s.Retention -- confirming that a Retention-only wiring (without a
// MessageStore) would still 404 through the real daemon is exactly the
// kind of gap this test file exists to catch.
func TestDaemonHandler_RetentionCandidatesRouteReachable(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/retention-route.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := &Server{
		Catalog:      stubCatalogLoader{},
		MessageStore: st.MessagingStore(),
		Retention:    st,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/messages/retention/candidates")
	if err != nil {
		t.Fatalf("GET /messages/retention/candidates: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (route must be mounted through the real daemon.Server.Handler())", resp.StatusCode)
	}
}

// TestDaemonHandler_MessageTraceRedriveAndPurgeRoutesReachable is T09's
// remaining instance of this same regression class, for the other three
// /messages/{id}/... actions this task adds: trace, redrive, purge.
// TestDaemonHandler_RetentionCandidatesRouteReachable above only proved
// the candidates route; this closes the gap the independent review of
// this diff flagged -- trace/redrive/purge had no equivalent
// daemon.Server.Handler()-level proof, only internal/api-level tests.
func TestDaemonHandler_MessageTraceRedriveAndPurgeRoutesReachable(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/messages-route.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := &Server{
		Catalog:        stubCatalogLoader{},
		MessageStore:   st.MessagingStore(),
		DeliveryTrace:  st,
		DeliveryRepair: st,
		Retention:      st,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	ctx := context.Background()
	from := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}

	// --- trace ---
	traceMsg, err := st.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: to})
	if err != nil {
		t.Fatalf("send (trace fixture): %v", err)
	}
	traceResp, err := http.Get(ts.URL + "/messages/" + traceMsg.ID + "/trace")
	if err != nil {
		t.Fatalf("GET trace: %v", err)
	}
	defer traceResp.Body.Close()
	if traceResp.StatusCode != http.StatusOK {
		t.Fatalf("trace status = %d, want 200 (route must be mounted through the real daemon.Server.Handler())", traceResp.StatusCode)
	}

	// --- redrive (needs a dead-lettered delivery) ---
	redriveMsg, err := st.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: to})
	if err != nil {
		t.Fatalf("send (redrive fixture): %v", err)
	}
	deliveryID, ok, err := st.DeliveryIDForMessage(ctx, redriveMsg.ID)
	if err != nil || !ok {
		t.Fatalf("delivery id: ok=%v err=%v", ok, err)
	}
	ds := st.DeliveryStore()
	claim, err := ds.Claim(ctx, delivery.ClaimRequest{DeliveryID: delivery.DeliveryID(deliveryID), Holder: "h1", LeaseDuration: 30 * time.Second, Nowait: true})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	lease := delivery.LeaseRef{DeliveryID: claim.Attempt.DeliveryID, AttemptID: claim.Attempt.ID, LeaseToken: claim.Attempt.LeaseToken}
	if _, _, err := ds.Nack(ctx, delivery.NackRequest{Lease: lease, Retryable: false, Error: "test"}); err != nil && !errors.Is(err, delivery.ErrDeadLettered) {
		t.Fatalf("nack: %v", err)
	}
	redriveResp, err := http.Post(ts.URL+"/messages/"+redriveMsg.ID+"/redrive", "application/json",
		bytes.NewReader([]byte(`{"authorized_by":"msg://agent/test/operator"}`)))
	if err != nil {
		t.Fatalf("POST redrive: %v", err)
	}
	defer redriveResp.Body.Close()
	if redriveResp.StatusCode != http.StatusOK {
		t.Fatalf("redrive status = %d, want 200 (route must be mounted through the real daemon.Server.Handler())", redriveResp.StatusCode)
	}

	// --- purge (needs a delivered message) ---
	purgeMsg, err := st.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: to})
	if err != nil {
		t.Fatalf("send (purge fixture): %v", err)
	}
	if err := st.MessagingStore().Consume(ctx, purgeMsg.ID, to); err != nil {
		t.Fatalf("consume: %v", err)
	}
	purgeResp, err := http.Post(ts.URL+"/messages/"+purgeMsg.ID+"/purge", "application/json",
		bytes.NewReader([]byte(`{"authorized_by":"msg://agent/test/operator"}`)))
	if err != nil {
		t.Fatalf("POST purge: %v", err)
	}
	defer purgeResp.Body.Close()
	if purgeResp.StatusCode != http.StatusOK {
		t.Fatalf("purge status = %d, want 200 (route must be mounted through the real daemon.Server.Handler())", purgeResp.StatusCode)
	}
}
