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
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
