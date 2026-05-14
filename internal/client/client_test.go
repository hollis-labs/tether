package client

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hollis-labs/go-agent-sessions/agentsessions"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/store"
)

// newMockDaemon spins up a real httptest.Server with the daemon handlers
// wired to a fake LaunchService. Returns the Client pointed at it and
// helpers for assertions. httptest.Server is TCP; we use tcp: listen
// address semantics so BaseURL + DialHTTPClient work unchanged.
type mockDaemon struct {
	server        *httptest.Server
	create        func(string) (api.LaunchResult, error)
	launchSession func(string) (api.LaunchResult, error)
	list          func() ([]store.SessionRow, error)
	get           func(string) (*store.SessionRow, error)
	stop          func(string) error
	wait          func(context.Context, string) (int, error)
	input         func(string, []byte) error
	attach        func(context.Context, string, io.Writer) error
	resize        func(string, uint16, uint16) error
	catalog       func() (*config.Catalog, error)
}

func (m *mockDaemon) addr() string {
	return "tcp:" + strings.TrimPrefix(m.server.URL, "http://")
}

func newMockDaemon(t *testing.T) *mockDaemon {
	t.Helper()
	m := &mockDaemon{}

	svc := &funcService{
		createFn: func(id string) (api.LaunchResult, error) {
			if m.create != nil {
				return m.create(id)
			}
			return api.LaunchResult{}, errors.New("create not configured")
		},
		launchFn: func(id string) (api.LaunchResult, error) {
			if m.launchSession != nil {
				return m.launchSession(id)
			}
			return api.LaunchResult{}, errors.New("launch not configured")
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
		inputFn: func(id string, data []byte) error {
			if m.input != nil {
				return m.input(id, data)
			}
			return errors.New("input not configured")
		},
		attachFn: func(ctx context.Context, id string, w io.Writer) error {
			if m.attach != nil {
				return m.attach(ctx, id, w)
			}
			return errors.New("attach not configured")
		},
		resizeFn: func(id string, rows, cols uint16) error {
			if m.resize != nil {
				return m.resize(id, rows, cols)
			}
			return errors.New("resize not configured")
		},
	}

	catalogLoader := mockCatalogLoader{fn: func() (*config.Catalog, error) {
		if m.catalog != nil {
			return m.catalog()
		}
		return nil, errors.New("catalog not configured")
	}}

	srv := &daemon.Server{Service: svc, Catalog: catalogLoader}
	m.server = httptest.NewServer(srv.Handler())
	t.Cleanup(func() { m.server.Close() })
	return m
}

// mockCatalogLoader is the client-test-side api.CatalogLoader: defers
// to a per-test closure set on mockDaemon.catalog.
type mockCatalogLoader struct {
	fn func() (*config.Catalog, error)
}

func (m mockCatalogLoader) Load() (*config.Catalog, error) { return m.fn() }

// funcService is a func-table LaunchService — lighter than a struct-full-of-
// fields fake for the case-by-case per-test overrides that client tests need.
type funcService struct {
	createFn   func(string) (api.LaunchResult, error)
	launchFn   func(string) (api.LaunchResult, error)
	listFn     func() ([]store.SessionRow, error)
	getFn      func(string) (*store.SessionRow, error)
	stopFn     func(string) error
	waitFn     func(context.Context, string) (int, error)
	inputFn    func(string, []byte) error
	attachFn   func(context.Context, string, io.Writer) error
	attachedFn func(string) int
	resizeFn   func(string, uint16, uint16) error
}

func (s *funcService) CreateSession(id string) (api.LaunchResult, error) {
	return s.createFn(id)
}
func (s *funcService) CreateSessionWithBootPrompt(id, _ string) (api.LaunchResult, error) {
	return s.createFn(id)
}
func (s *funcService) CreateSessionWithInput(in api.CreateSessionInput) (api.LaunchResult, error) {
	return s.createFn(in.LaunchID)
}
func (s *funcService) LaunchSession(id string) (api.LaunchResult, error) {
	return s.launchFn(id)
}
func (s *funcService) ListSessions(_ store.ListSessionsOptions) ([]store.SessionRow, error) {
	return s.listFn()
}
func (s *funcService) GetSession(id string) (*store.SessionRow, error) {
	return s.getFn(id)
}
func (s *funcService) StopSession(id string) error { return s.stopFn(id) }
func (s *funcService) WaitSession(ctx context.Context, id string) (int, error) {
	return s.waitFn(ctx, id)
}
func (s *funcService) SendInput(id string, data []byte) error { return s.inputFn(id, data) }
func (s *funcService) SendTurn(_ context.Context, id, text string) error {
	return s.inputFn(id, []byte(text))
}
func (s *funcService) AttachSession(ctx context.Context, id string, w io.Writer, _ int64) error {
	return s.attachFn(ctx, id, w)
}
func (s *funcService) AttachedClients(id string) int {
	if s.attachedFn == nil {
		return 0
	}
	return s.attachedFn(id)
}
func (s *funcService) ResizeSession(id string, rows, cols uint16) error {
	if s.resizeFn == nil {
		return nil
	}
	return s.resizeFn(id, rows, cols)
}

func (s *funcService) ResumeLogicalAgent(_ string) (api.LaunchResult, error) {
	return api.LaunchResult{}, nil
}

func (s *funcService) RuntimeHealth(_ string) (api.RuntimeHealthResult, bool) {
	return api.RuntimeHealthResult{}, false
}

func TestClient_ResizeSession(t *testing.T) {
	var gotID string
	var gotRows, gotCols uint16
	m := newMockDaemon(t)
	m.resize = func(id string, rows, cols uint16) error {
		gotID = id
		gotRows, gotCols = rows, cols
		return nil
	}
	c := New(m.addr())
	if err := c.ResizeSession(context.Background(), "s1", 42, 120); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotID != "s1" || gotRows != 42 || gotCols != 120 {
		t.Errorf("resize dispatch = (%q, %d, %d); want (s1, 42, 120)", gotID, gotRows, gotCols)
	}
}

func TestClient_ResizeSession_NotRunning(t *testing.T) {
	m := newMockDaemon(t)
	m.resize = func(string, uint16, uint16) error { return agentsessions.ErrSessionNotRunning }
	c := New(m.addr())
	err := c.ResizeSession(context.Background(), "s1", 24, 80)
	if err == nil {
		t.Fatal("expected error for not-running session")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("expected 404 in error, got %v", err)
	}
}

func TestClient_CreateSession(t *testing.T) {
	m := newMockDaemon(t)
	m.create = func(id string) (api.LaunchResult, error) {
		if id != "demo" {
			t.Errorf("create id = %q", id)
		}
		return api.LaunchResult{SessionID: "sess-1", Workspace: "/ws", LogPath: "/ws/logs/session.log"}, nil
	}
	c := New(m.addr())
	res, err := c.CreateSession(context.Background(), "demo")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if res.ID != "sess-1" {
		t.Errorf("ID = %q", res.ID)
	}
}

func TestClient_LaunchSession(t *testing.T) {
	m := newMockDaemon(t)
	m.launchSession = func(id string) (api.LaunchResult, error) {
		if id != "sess-1" {
			t.Errorf("launch id = %q", id)
		}
		return api.LaunchResult{SessionID: id, Workspace: "/ws", LogPath: "/ws/logs/session.log"}, nil
	}
	c := New(m.addr())
	res, err := c.LaunchSession(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("LaunchSession: %v", err)
	}
	if res.ID != "sess-1" {
		t.Errorf("ID = %q", res.ID)
	}
}

func TestClient_Launch_CreateAndLaunch(t *testing.T) {
	m := newMockDaemon(t)
	m.create = func(id string) (api.LaunchResult, error) {
		return api.LaunchResult{SessionID: "sess-1", Workspace: "/ws", LogPath: "/ws/logs/session.log"}, nil
	}
	var launchedID string
	m.launchSession = func(id string) (api.LaunchResult, error) {
		launchedID = id
		return api.LaunchResult{SessionID: id, Workspace: "/ws", LogPath: "/ws/logs/session.log"}, nil
	}
	c := New(m.addr())
	res, err := c.Launch(context.Background(), "demo")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if res.ID != "sess-1" {
		t.Errorf("ID = %q", res.ID)
	}
	if launchedID != "sess-1" {
		t.Errorf("launch session id = %q, want sess-1", launchedID)
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
	res, err := c.ListSessions(context.Background(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sessions) != 2 {
		t.Fatalf("len = %d", len(res.Sessions))
	}
	if res.Sessions[0].PID == nil || *res.Sessions[0].PID != 9 {
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
	if _, err := c.ListSessions(context.Background(), ListOptions{}); !errors.Is(err, ErrDaemonUnreachable) {
		t.Errorf("ListSessions: expected ErrDaemonUnreachable; got %v", err)
	}
}

func TestClient_SendInput(t *testing.T) {
	m := newMockDaemon(t)
	var got []byte
	var mu sync.Mutex
	m.input = func(id string, data []byte) error {
		mu.Lock()
		defer mu.Unlock()
		if id != "s1" {
			t.Errorf("id = %q", id)
		}
		got = append([]byte(nil), data...)
		return nil
	}
	c := New(m.addr())
	if err := c.SendInput(context.Background(), "s1", []byte("echo\n")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if string(got) != "echo\n" {
		t.Errorf("daemon received %q, want %q", got, "echo\n")
	}
}

func TestClient_SendInput_DaemonError(t *testing.T) {
	m := newMockDaemon(t)
	m.input = func(string, []byte) error { return errors.New("not running") }
	c := New(m.addr())
	if err := c.SendInput(context.Background(), "s1", []byte("x")); err == nil {
		t.Fatal("expected error from daemon")
	}
}

func TestClient_AttachSession_StreamsBytes(t *testing.T) {
	m := newMockDaemon(t)
	m.attach = func(_ context.Context, id string, w io.Writer) error {
		_, _ = w.Write([]byte("alpha-"))
		_, _ = w.Write([]byte("beta"))
		return nil
	}
	c := New(m.addr())
	var buf bytes.Buffer
	if err := c.AttachSession(context.Background(), "s1", &buf, 0); err != nil {
		t.Fatalf("AttachSession: %v", err)
	}
	if buf.String() != "alpha-beta" {
		t.Errorf("got %q, want %q", buf.String(), "alpha-beta")
	}
}

func TestClient_AttachSession_DaemonUnreachable(t *testing.T) {
	c := New("tcp:127.0.0.1:1")
	err := c.AttachSession(context.Background(), "s1", io.Discard, 0)
	if !errors.Is(err, ErrDaemonUnreachable) {
		t.Errorf("expected ErrDaemonUnreachable; got %v", err)
	}
}

func TestClient_UnreachableUnixSocket(t *testing.T) {
	c := New("unix:/tmp/mux-nonexistent-" + t.Name() + ".sock")
	if err := c.Ping(context.Background()); !errors.Is(err, ErrDaemonUnreachable) {
		t.Errorf("expected ErrDaemonUnreachable; got %v", err)
	}
}

func TestClient_ListProjects(t *testing.T) {
	m := newMockDaemon(t)
	m.catalog = func() (*config.Catalog, error) {
		return &config.Catalog{
			Projects: map[string]config.Project{
				"p1": {ID: "p1", Name: "First"},
				"p2": {ID: "p2", Name: "Second"},
			},
		}, nil
	}
	c := New(m.addr())
	projects, err := c.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("len = %d", len(projects))
	}
}

func TestClient_ListAgents(t *testing.T) {
	m := newMockDaemon(t)
	m.catalog = func() (*config.Catalog, error) {
		return &config.Catalog{
			Agents: map[string]config.Agent{"a1": {ID: "a1", Name: "Alpha"}},
		}, nil
	}
	c := New(m.addr())
	agents, err := c.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 1 || agents[0].ID != "a1" {
		t.Errorf("agents = %+v", agents)
	}
}

func TestClient_ListProviders(t *testing.T) {
	m := newMockDaemon(t)
	m.catalog = func() (*config.Catalog, error) {
		return &config.Catalog{
			Providers: map[string]config.Provider{"stub": {ID: "stub", Type: "api"}},
		}, nil
	}
	c := New(m.addr())
	providers, err := c.ListProviders(context.Background())
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if len(providers) != 1 || providers[0].ID != "stub" {
		t.Errorf("providers = %+v", providers)
	}
}

func TestClient_ListLaunches(t *testing.T) {
	m := newMockDaemon(t)
	m.catalog = func() (*config.Catalog, error) {
		return &config.Catalog{
			Launches: map[string]config.Launch{
				"demo-launch": {ID: "demo-launch", Project: "demo", Agent: "demo-agent", Provider: "claude-code"},
			},
		}, nil
	}
	c := New(m.addr())
	launches, err := c.ListLaunches(context.Background())
	if err != nil {
		t.Fatalf("ListLaunches: %v", err)
	}
	if len(launches) != 1 || launches[0].ID != "demo-launch" {
		t.Errorf("launches = %+v", launches)
	}
}

func TestClient_ListProjects_DaemonError(t *testing.T) {
	m := newMockDaemon(t)
	m.catalog = func() (*config.Catalog, error) {
		return nil, errors.New("parse /catalog/global.yaml: bad yaml")
	}
	c := New(m.addr())
	if _, err := c.ListProjects(context.Background()); err == nil {
		t.Fatal("expected error from daemon 500")
	}
}

func TestClient_Catalog_Unreachable(t *testing.T) {
	c := New("tcp:127.0.0.1:1")
	for _, fn := range []func() error{
		func() error { _, err := c.ListProjects(context.Background()); return err },
		func() error { _, err := c.ListAgents(context.Background()); return err },
		func() error { _, err := c.ListProviders(context.Background()); return err },
		func() error { _, err := c.ListLaunches(context.Background()); return err },
	} {
		if err := fn(); !errors.Is(err, ErrDaemonUnreachable) {
			t.Errorf("expected ErrDaemonUnreachable; got %v", err)
		}
	}
}
