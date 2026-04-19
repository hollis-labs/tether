package client

import (
	"context"
	"database/sql"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chrispian/agent-mux/internal/daemon"
	"github.com/chrispian/agent-mux/internal/store"
)

// newMockDaemon spins up a real httptest.Server with the daemon handlers
// wired to a fake LaunchService. Returns the Client pointed at it and
// helpers for assertions. httptest.Server is TCP; we use tcp: listen
// address semantics so BaseURL + DialHTTPClient work unchanged.
type mockDaemon struct {
	server *httptest.Server
	launch func(string) (daemon.LaunchResult, error)
	list   func() ([]store.SessionRow, error)
	get    func(string) (*store.SessionRow, error)
	stop   func(string) error
	wait   func(context.Context, string) (int, error)
}

func (m *mockDaemon) addr() string {
	return "tcp:" + strings.TrimPrefix(m.server.URL, "http://")
}

func newMockDaemon(t *testing.T) *mockDaemon {
	t.Helper()
	m := &mockDaemon{}

	svc := &funcService{
		launchFn: func(id string) (daemon.LaunchResult, error) {
			if m.launch != nil {
				return m.launch(id)
			}
			return daemon.LaunchResult{}, errors.New("launch not configured")
		},
		listFn: func() ([]store.SessionRow, error) {
			if m.list != nil {
				return m.list()
			}
			return nil, errors.New("list not configured")
		},
		getFn: func(id string) (*store.SessionRow, error) {
			if m.get != nil {
				return m.get(id)
			}
			return nil, errors.New("get not configured")
		},
		stopFn: func(id string) error {
			if m.stop != nil {
				return m.stop(id)
			}
			return errors.New("stop not configured")
		},
		waitFn: func(ctx context.Context, id string) (int, error) {
			if m.wait != nil {
				return m.wait(ctx, id)
			}
			return 0, errors.New("wait not configured")
		},
	}

	srv := &daemon.Server{Service: svc}
	m.server = httptest.NewServer(srv.Handler())
	t.Cleanup(func() { m.server.Close() })
	return m
}

// funcService is a func-table LaunchService — lighter than a struct-full-of-
// fields fake for the case-by-case per-test overrides that client tests need.
type funcService struct {
	launchFn func(string) (daemon.LaunchResult, error)
	listFn   func() ([]store.SessionRow, error)
	getFn    func(string) (*store.SessionRow, error)
	stopFn   func(string) error
	waitFn   func(context.Context, string) (int, error)
}

func (s *funcService) Launch(id string) (daemon.LaunchResult, error) { return s.launchFn(id) }
func (s *funcService) ListSessions() ([]store.SessionRow, error)     { return s.listFn() }
func (s *funcService) GetSession(id string) (*store.SessionRow, error) {
	return s.getFn(id)
}
func (s *funcService) StopSession(id string) error { return s.stopFn(id) }
func (s *funcService) WaitSession(ctx context.Context, id string) (int, error) {
	return s.waitFn(ctx, id)
}

func TestClient_Launch(t *testing.T) {
	m := newMockDaemon(t)
	m.launch = func(id string) (daemon.LaunchResult, error) {
		if id != "demo" {
			t.Errorf("launch id = %q", id)
		}
		return daemon.LaunchResult{SessionID: "sess-1", Workspace: "/ws", LogPath: "/ws/logs/session.log"}, nil
	}
	c := New(m.addr())
	res, err := c.Launch(context.Background(), "demo")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if res.ID != "sess-1" {
		t.Errorf("ID = %q", res.ID)
	}
}

func TestClient_ListSessions(t *testing.T) {
	m := newMockDaemon(t)
	m.list = func() ([]store.SessionRow, error) {
		return []store.SessionRow{
			{ID: "a", State: "running", PID: sql.NullInt64{Int64: 9, Valid: true}},
			{ID: "b", State: "completed"},
		}, nil
	}
	c := New(m.addr())
	dtos, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(dtos) != 2 {
		t.Fatalf("len = %d", len(dtos))
	}
	if dtos[0].PID == nil || *dtos[0].PID != 9 {
		t.Errorf("PID missing on first row")
	}
}

func TestClient_GetSession(t *testing.T) {
	m := newMockDaemon(t)
	m.get = func(id string) (*store.SessionRow, error) {
		return &store.SessionRow{ID: id, State: "running"}, nil
	}
	c := New(m.addr())
	dto, err := c.GetSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if dto.ID != "s1" || dto.State != "running" {
		t.Errorf("dto = %+v", dto)
	}
}

func TestClient_StopSession(t *testing.T) {
	m := newMockDaemon(t)
	called := 0
	m.stop = func(id string) error { called++; return nil }
	c := New(m.addr())
	if err := c.StopSession(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Errorf("StopSession not dispatched; called=%d", called)
	}
}

func TestClient_WaitSession(t *testing.T) {
	m := newMockDaemon(t)
	m.wait = func(_ context.Context, id string) (int, error) { return 7, nil }
	c := New(m.addr())
	code, err := c.WaitSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 {
		t.Errorf("exit = %d", code)
	}
}

func TestClient_Ping(t *testing.T) {
	m := newMockDaemon(t)
	c := New(m.addr())
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestClient_Unreachable(t *testing.T) {
	c := New("tcp:127.0.0.1:1") // nothing listening
	err := c.Ping(context.Background())
	if !errors.Is(err, ErrDaemonUnreachable) {
		t.Errorf("expected ErrDaemonUnreachable; got %v", err)
	}

	// Also verify high-level methods wrap unreachable correctly.
	if _, err := c.ListSessions(context.Background()); !errors.Is(err, ErrDaemonUnreachable) {
		t.Errorf("ListSessions: expected ErrDaemonUnreachable; got %v", err)
	}
}

func TestClient_UnreachableUnixSocket(t *testing.T) {
	c := New("unix:/tmp/mux-nonexistent-" + t.Name() + ".sock")
	if err := c.Ping(context.Background()); !errors.Is(err, ErrDaemonUnreachable) {
		t.Errorf("expected ErrDaemonUnreachable; got %v", err)
	}
}
