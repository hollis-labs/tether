package daemon

// a2a_route_test.go — T10 (messaging vNext, CW-20260906-0041) regression
// test in the same class whoami_route_test.go documents: a route
// correctly served by its own package can still 404 through the real
// production daemon if this package's Handler() doesn't mount it. A2A is
// mounted as a bare http.Handler (Server.A2A), not through api.NewHandler
// at all, so it needs its own proof distinct from the api-Deps-driven
// routes whoami_route_test.go covers.

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/a2aadapter"
	"github.com/hollis-labs/tether/internal/store"
)

func TestDaemonHandler_A2ARouteReachable(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "a2a-route.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Two-phase construction (URL needed before the AgentCard can be
	// built, but the AgentCard's URL is needed before the server can be
	// given its real handler) -- same pattern internal/a2aadapter's own
	// fixture tests use.
	ts := httptest.NewServer(nil)
	t.Cleanup(ts.Close)

	adapter, err := a2aadapter.NewAdapter(a2aadapter.Config{Bindings: []a2aadapter.AgentBinding{
		{ID: "route-test", TargetURN: "msg://agent/agent-mux/agt_routetest0", BaseURL: ts.URL + "/a2a"},
	}}, db.MessagingStore())
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}

	srv := &Server{Catalog: stubCatalogLoader{}, A2A: adapter.Mux()}
	ts.Config.Handler = srv.Handler()

	resp, err := http.Get(ts.URL + "/a2a/agents/route-test/.well-known/agent-card.json")
	if err != nil {
		t.Fatalf("GET agent card: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (route must be mounted through the real daemon.Server.Handler(), not just a2aadapter.Adapter.Mux() directly)", resp.StatusCode)
	}
}

func TestDaemonHandler_A2AAbsentByDefault(t *testing.T) {
	srv := &Server{Catalog: stubCatalogLoader{}}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/a2a/agents/anything/.well-known/agent-card.json")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (A2A absent when Server.A2A is nil -- T10 acceptance #3: optional for local messaging)", resp.StatusCode)
	}
}
