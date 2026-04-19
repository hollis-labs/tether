package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chrispian/agent-mux/internal/runtime"
	"github.com/chrispian/agent-mux/internal/store"
)

// fakeLaunchService is a LaunchService stub for handler tests. Each method
// records how it was called so assertions can check routing behaviour.
type fakeLaunchService struct {
	mu sync.Mutex

	launchRes LaunchResult
	launchErr error
	launchIDs []string

	listRes []store.SessionRow
	listErr error

	getRes  map[string]*store.SessionRow
	getErr  error
	stopErr error
	stopIDs []string
	waitRes map[string]int
	waitErr error

	inputErr  error
	inputLog  [][]byte
	inputIDs  []string
	attachFn  func(ctx context.Context, id string, w io.Writer) error
	attachErr error

	attachedClients map[string]int
}

func (f *fakeLaunchService) Launch(id string) (LaunchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launchIDs = append(f.launchIDs, id)
	return f.launchRes, f.launchErr
}
func (f *fakeLaunchService) ListSessions() ([]store.SessionRow, error) {
	return f.listRes, f.listErr
}
func (f *fakeLaunchService) GetSession(id string) (*store.SessionRow, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	r, ok := f.getRes[id]
	if !ok {
		return nil, errors.New("sql: no rows in result set")
	}
	return r, nil
}
func (f *fakeLaunchService) StopSession(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopIDs = append(f.stopIDs, id)
	return f.stopErr
}
func (f *fakeLaunchService) WaitSession(_ context.Context, id string) (int, error) {
	if f.waitErr != nil {
		return 0, f.waitErr
	}
	return f.waitRes[id], nil
}

func (f *fakeLaunchService) SendInput(id string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputIDs = append(f.inputIDs, id)
	cp := make([]byte, len(data))
	copy(cp, data)
	f.inputLog = append(f.inputLog, cp)
	return f.inputErr
}

func (f *fakeLaunchService) AttachSession(ctx context.Context, id string, w io.Writer) error {
	if f.attachFn != nil {
		return f.attachFn(ctx, id, w)
	}
	return f.attachErr
}

func (f *fakeLaunchService) AttachedClients(id string) int {
	if f.attachedClients == nil {
		return 0
	}
	return f.attachedClients[id]
}

func newTestServer(svc LaunchService) *Server {
	return &Server{
		Config:  Config{ListenAddr: "tcp:127.0.0.1:0"},
		Service: svc,
	}
}

func buildRouter(s *Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	s.registerSessionRoutes(mux)
	return mux
}

func TestHandleLaunch_Success(t *testing.T) {
	svc := &fakeLaunchService{launchRes: LaunchResult{SessionID: "sess-1", Workspace: "/ws/1", LogPath: "/ws/1/logs/session.log"}}
	srv := newTestServer(svc)

	body, _ := json.Marshal(LaunchRequest{Launch: "demo-launch"})
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	buildRouter(srv).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rr.Code, rr.Body.String())
	}
	var res LaunchResponse
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res.ID != "sess-1" {
		t.Errorf("id = %q", res.ID)
	}
	if res.Workspace != "/ws/1" {
		t.Errorf("workspace = %q", res.Workspace)
	}
	if len(svc.launchIDs) != 1 || svc.launchIDs[0] != "demo-launch" {
		t.Errorf("Launch not dispatched with request body: %v", svc.launchIDs)
	}
}

func TestHandleLaunch_MissingID(t *testing.T) {
	svc := &fakeLaunchService{}
	srv := newTestServer(svc)
	body, _ := json.Marshal(LaunchRequest{})
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	buildRouter(srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestHandleLaunch_ServiceError(t *testing.T) {
	svc := &fakeLaunchService{launchErr: errors.New("adapter unavailable")}
	srv := newTestServer(svc)
	body, _ := json.Marshal(LaunchRequest{Launch: "demo"})
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	buildRouter(srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "adapter unavailable") {
		t.Errorf("error message missing: %s", rr.Body.String())
	}
}

func TestHandleListSessions(t *testing.T) {
	rows := []store.SessionRow{
		{ID: "s1", State: "running", PID: sql.NullInt64{Int64: 42, Valid: true}},
		{ID: "s2", State: "completed", ExitCode: sql.NullInt64{Int64: 0, Valid: true}},
	}
	svc := &fakeLaunchService{listRes: rows}
	srv := newTestServer(svc)
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	rr := httptest.NewRecorder()
	buildRouter(srv).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var res ListSessionsResponse
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if len(res.Sessions) != 2 {
		t.Fatalf("sessions = %d", len(res.Sessions))
	}
	if res.Sessions[0].PID == nil || *res.Sessions[0].PID != 42 {
		t.Errorf("PID flatten broken: %+v", res.Sessions[0])
	}
	if res.Sessions[1].ExitCode == nil || *res.Sessions[1].ExitCode != 0 {
		t.Errorf("ExitCode flatten broken: %+v", res.Sessions[1])
	}
}

func TestHandleGetSession_NotFound(t *testing.T) {
	svc := &fakeLaunchService{getRes: map[string]*store.SessionRow{}}
	srv := newTestServer(svc)
	req := httptest.NewRequest(http.MethodGet, "/sessions/missing", nil)
	rr := httptest.NewRecorder()
	buildRouter(srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestHandleGetSession_Found(t *testing.T) {
	row := &store.SessionRow{ID: "s1", State: "running"}
	svc := &fakeLaunchService{getRes: map[string]*store.SessionRow{"s1": row}}
	srv := newTestServer(svc)
	req := httptest.NewRequest(http.MethodGet, "/sessions/s1", nil)
	rr := httptest.NewRecorder()
	buildRouter(srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var dto SessionDTO
	if err := json.NewDecoder(rr.Body).Decode(&dto); err != nil {
		t.Fatal(err)
	}
	if dto.ID != "s1" || dto.State != "running" {
		t.Errorf("dto = %+v", dto)
	}
}

func TestHandleStopSession_Success(t *testing.T) {
	svc := &fakeLaunchService{}
	srv := newTestServer(svc)
	req := httptest.NewRequest(http.MethodPost, "/sessions/s1/stop", nil)
	rr := httptest.NewRecorder()
	buildRouter(srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204: %s", rr.Code, rr.Body.String())
	}
	if len(svc.stopIDs) != 1 || svc.stopIDs[0] != "s1" {
		t.Errorf("StopSession not dispatched: %v", svc.stopIDs)
	}
}

func TestHandleStopSession_NotRunning(t *testing.T) {
	svc := &fakeLaunchService{stopErr: runtime.ErrSessionNotRunning}
	srv := newTestServer(svc)
	req := httptest.NewRequest(http.MethodPost, "/sessions/s1/stop", nil)
	rr := httptest.NewRecorder()
	buildRouter(srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestHandleWaitSession(t *testing.T) {
	svc := &fakeLaunchService{waitRes: map[string]int{"s1": 42}}
	srv := newTestServer(svc)
	req := httptest.NewRequest(http.MethodGet, "/sessions/s1/wait", nil)
	rr := httptest.NewRecorder()
	buildRouter(srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var res WaitResponse
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42", res.ExitCode)
	}
}

func TestHandleWaitSession_NotRunning(t *testing.T) {
	svc := &fakeLaunchService{waitErr: runtime.ErrSessionNotRunning}
	srv := newTestServer(svc)
	req := httptest.NewRequest(http.MethodGet, "/sessions/s1/wait", nil)
	rr := httptest.NewRecorder()
	buildRouter(srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestHandleSessions_MethodNotAllowed(t *testing.T) {
	svc := &fakeLaunchService{}
	srv := newTestServer(svc)

	req := httptest.NewRequest(http.MethodDelete, "/sessions", nil)
	rr := httptest.NewRecorder()
	buildRouter(srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /sessions status = %d, want 405", rr.Code)
	}
}

func TestHandleSendInput_Success(t *testing.T) {
	svc := &fakeLaunchService{}
	srv := newTestServer(svc)
	req := httptest.NewRequest(http.MethodPost, "/sessions/s1/input", bytes.NewReader([]byte("hello\n")))
	rr := httptest.NewRecorder()
	buildRouter(srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rr.Code, rr.Body.String())
	}
	if len(svc.inputIDs) != 1 || svc.inputIDs[0] != "s1" {
		t.Errorf("SendInput not dispatched: %v", svc.inputIDs)
	}
	if string(svc.inputLog[0]) != "hello\n" {
		t.Errorf("inputLog[0] = %q", svc.inputLog[0])
	}
}

func TestHandleSendInput_SessionNotRunning(t *testing.T) {
	svc := &fakeLaunchService{inputErr: runtime.ErrSessionNotRunning}
	srv := newTestServer(svc)
	req := httptest.NewRequest(http.MethodPost, "/sessions/s1/input", bytes.NewReader([]byte("x")))
	rr := httptest.NewRecorder()
	buildRouter(srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestHandleSendInput_NoPTYConflict(t *testing.T) {
	svc := &fakeLaunchService{inputErr: runtime.ErrNoPTYWriter}
	srv := newTestServer(svc)
	req := httptest.NewRequest(http.MethodPost, "/sessions/s1/input", bytes.NewReader([]byte("x")))
	rr := httptest.NewRecorder()
	buildRouter(srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rr.Code)
	}
}

func TestHandleSendInput_TooLarge(t *testing.T) {
	svc := &fakeLaunchService{}
	srv := newTestServer(svc)
	body := bytes.Repeat([]byte{'x'}, maxInputBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/sessions/s1/input", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	buildRouter(srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rr.Code)
	}
	if len(svc.inputLog) != 0 {
		t.Errorf("body was forwarded despite being too large: %d entries", len(svc.inputLog))
	}
}

func TestHandleAttach_StreamsUntilServiceReturns(t *testing.T) {
	svc := &fakeLaunchService{
		attachFn: func(ctx context.Context, id string, w io.Writer) error {
			if _, err := w.Write([]byte("first ")); err != nil {
				return err
			}
			if _, err := w.Write([]byte("second")); err != nil {
				return err
			}
			return nil
		},
	}
	srv := newTestServer(svc)
	req := httptest.NewRequest(http.MethodGet, "/sessions/s1/attach", nil)
	rr := httptest.NewRecorder()
	buildRouter(srv).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if rr.Body.String() != "first second" {
		t.Errorf("body = %q, want %q", rr.Body.String(), "first second")
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestHandleAttach_SessionNotRunning(t *testing.T) {
	svc := &fakeLaunchService{attachErr: runtime.ErrSessionNotRunning}
	srv := newTestServer(svc)
	// With a real server we'd get 404 before streaming; our handler already
	// wrote 200 + headers, so the check is that the stream terminates cleanly
	// with no body and the handler returns.
	req := httptest.NewRequest(http.MethodGet, "/sessions/s1/attach", nil)
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { buildRouter(srv).ServeHTTP(rr, req); close(done) }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("attach handler did not return")
	}
}

func TestSessionRoutes_NotRegisteredWithoutService(t *testing.T) {
	srv := newTestServer(nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.handleHealth)
	srv.registerSessionRoutes(mux) // no-op when Service is nil

	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 when Service is nil; got %d", rr.Code)
	}
}
