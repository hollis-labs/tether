package daemon

// chat_route_test.go — the chat roster routes (CW-20261012-0099) shipped in
// PR #250 registered in internal/api but were never mounted by the daemon, so
// GET /chat/agents answered 404 on the real hub while the api-level tests
// (which build api.NewHandler directly) passed. This drives the real
// daemon.Server.Handler(), the only thing production serves.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/store"
)

// chatTestService satisfies api.LaunchService by embedding the interface; the
// roster list only calls ListSessions, and any other call would panic loudly.
type chatTestService struct{ api.LaunchService }

func (chatTestService) ListSessions(store.ListSessionsOptions) ([]store.SessionRow, error) {
	return nil, nil
}

type chatTestCatalog struct{}

func (chatTestCatalog) Load() (*config.Catalog, error) {
	return &config.Catalog{
		Launches: map[string]config.Launch{
			"pm":    {ID: "pm", Agent: "portfolio-manager", Chat: true, DisplayName: "Portfolio Manager"},
			"other": {ID: "other", Agent: "worker"},
		},
	}, nil
}

func TestChatRosterReachableThroughDaemon(t *testing.T) {
	server := &Server{Service: chatTestService{}, Catalog: chatTestCatalog{}}
	handler := server.Handler()

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/chat/agents", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /chat/agents through the daemon = %d: %s", w.Code, w.Body.String())
	}
	var body api.ListChatAgentsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Agents) != 1 || body.Agents[0].LaunchID != "pm" || body.Agents[0].DisplayName != "Portfolio Manager" {
		t.Fatalf("roster = %+v, want only the chat: true launch", body.Agents)
	}
}

func TestChatSendRouteMountedThroughDaemon(t *testing.T) {
	server := &Server{Service: chatTestService{}, Catalog: chatTestCatalog{}}
	handler := server.Handler()

	// GET on the send path is refused with 405 by the api handler. A 404 means
	// the subtree was never mounted and POSTs would never reach it either.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/chat/agents/pm/send", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /chat/agents/pm/send through the daemon = %d (404 means unmounted): %s", w.Code, w.Body.String())
	}
}
