package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/config"
)

// newTestClient wires a tui/client against an httptest.Server using
// the tcp: listen-addr form so daemon.DialHTTPClient / daemon.BaseURL
// treat it as a plain HTTP target.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	t.Cleanup(srv.Close)
	addr := "tcp:" + strings.TrimPrefix(srv.URL, "http://")
	return New(addr)
}

func TestListProjects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/catalog/projects" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(api.ListProjectsResponse{
			Projects: []config.Project{{ID: "acme"}, {ID: "beta"}},
		})
	}))
	c := newTestClient(t, srv)

	got, err := c.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0].ID != "acme" || got[1].ID != "beta" {
		t.Fatalf("unexpected projects: %+v", got)
	}
}

func TestListAgents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/catalog/agents" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(api.ListAgentsResponse{
			Agents: []config.Agent{{ID: "writer"}},
		})
	}))
	c := newTestClient(t, srv)

	got, err := c.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "writer" {
		t.Fatalf("unexpected agents: %+v", got)
	}
}

func TestListProviders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ListProvidersResponse{
			Providers: []config.Provider{{ID: "stub"}, {ID: "claudecode"}},
		})
	}))
	c := newTestClient(t, srv)

	got, err := c.ListProviders(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(got))
	}
}

func TestListLaunches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ListLaunchesResponse{
			Launches: []config.Launch{{ID: "demo-launch"}},
		})
	}))
	c := newTestClient(t, srv)

	got, err := c.ListLaunches(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "demo-launch" {
		t.Fatalf("unexpected launches: %+v", got)
	}
}

func TestListSessions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ListSessionsResponse{
			Sessions: []api.SessionDTO{
				{ID: "s1", State: "running", ProjectID: "acme"},
			},
		})
	}))
	c := newTestClient(t, srv)

	got, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "s1" {
		t.Fatalf("unexpected sessions: %+v", got)
	}
}

func TestCreateAndLaunch(t *testing.T) {
	var createPayload api.LaunchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sessions":
			_ = json.NewDecoder(r.Body).Decode(&createPayload)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(api.LaunchResponse{
				ID:        "sess-xyz",
				Workspace: "/tmp/ws",
				Log:       "/tmp/ws/logs/session.log",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/sessions/sess-xyz/launch":
			_ = json.NewEncoder(w).Encode(api.LaunchResponse{
				ID:        "sess-xyz",
				Workspace: "/tmp/ws",
				Log:       "/tmp/ws/logs/session.log",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	c := newTestClient(t, srv)

	got, err := c.CreateAndLaunch(context.Background(), CreateAndLaunchRequest{LaunchID: "demo-launch"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.SessionID != "sess-xyz" {
		t.Fatalf("expected sess-xyz, got %q", got.SessionID)
	}
	if got.Workspace != "/tmp/ws" {
		t.Fatalf("expected workspace /tmp/ws, got %q", got.Workspace)
	}
	if createPayload.Launch != "demo-launch" {
		t.Fatalf("expected create payload launch=demo-launch, got %q", createPayload.Launch)
	}
}

func TestCreateAndLaunchRejectsEmptyLaunchID(t *testing.T) {
	c := New("tcp:127.0.0.1:1") // never contacted
	_, err := c.CreateAndLaunch(context.Background(), CreateAndLaunchRequest{})
	if err == nil {
		t.Fatal("expected error for empty LaunchID")
	}
	if !strings.Contains(err.Error(), "launch_id is required") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestErrDaemonUnreachablePreserved(t *testing.T) {
	// Unused server; we point the client at a TCP address nobody listens on.
	c := New("tcp:127.0.0.1:1") // port 1 is unassigned / closed
	_, err := c.ListProjects(context.Background())
	if err == nil {
		t.Fatal("expected error dialing a dead daemon")
	}
	if !errors.Is(err, ErrDaemonUnreachable) {
		t.Fatalf("expected ErrDaemonUnreachable, got %v", err)
	}
	if !strings.Contains(err.Error(), "tui client:") {
		t.Fatalf("expected 'tui client:' prefix, got %v", err)
	}
}

func TestPing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	c := newTestClient(t, srv)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping failed: %v", err)
	}
}
