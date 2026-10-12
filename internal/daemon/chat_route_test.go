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
	"strings"
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

// chatSendService records the create input of a no-session chat send.
type chatSendService struct {
	api.LaunchService
	inputs   []api.CreateSessionInput
	launched []string
}

func (*chatSendService) ListSessions(store.ListSessionsOptions) ([]store.SessionRow, error) {
	return nil, nil
}

func (s *chatSendService) CreateSessionWithInput(in api.CreateSessionInput) (api.LaunchResult, error) {
	s.inputs = append(s.inputs, in)
	return api.LaunchResult{SessionID: "new"}, nil
}

func (s *chatSendService) LaunchSession(id string) (api.LaunchResult, error) {
	s.launched = append(s.launched, id)
	return api.LaunchResult{SessionID: id}, nil
}

type chatWorkerCatalog struct{}

func (chatWorkerCatalog) Load() (*config.Catalog, error) {
	return &config.Catalog{Launches: map[string]config.Launch{
		"pm":      {ID: "pm", Agent: "portfolio-manager", Chat: true, Worker: "agent-os"},
		"no-wrkr": {ID: "no-wrkr", Agent: "x", Chat: true},
	}}, nil
}

// A no-session send through the real handler must reach the launch service
// with the launch's worker, never a worker-less (hub-local) create.
func TestChatSendNoSessionUsesLaunchWorkerThroughDaemon(t *testing.T) {
	svc := &chatSendService{}
	handler := (&Server{Service: svc, Catalog: chatWorkerCatalog{}}).Handler()
	post := func(launch string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/chat/agents/"+launch+"/send", strings.NewReader(`{"text":"hello","idempotency_key":"k1"}`)))
		return w
	}
	if w := post("pm"); w.Code != http.StatusCreated {
		t.Fatalf("send = %d: %s", w.Code, w.Body.String())
	}
	if len(svc.inputs) != 1 || svc.inputs[0].Worker != "agent-os" || svc.inputs[0].LaunchID != "pm" || svc.inputs[0].PromptAppend != "hello" {
		t.Fatalf("create inputs = %+v, want Worker agent-os", svc.inputs)
	}
	if len(svc.launched) != 1 || svc.launched[0] != "new" {
		t.Fatalf("launched = %v", svc.launched)
	}

	w := post("no-wrkr")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "worker_not_configured") || len(svc.inputs) != 1 {
		t.Fatalf("no-worker send = %d: %s (creates %d)", w.Code, w.Body.String(), len(svc.inputs))
	}
}
