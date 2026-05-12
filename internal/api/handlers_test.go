package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/go-agent-sessions/agentsessions"

	"github.com/chrispian/agent-mux/internal/session"
	"github.com/chrispian/agent-mux/internal/store"
)

// fakeLaunchService is a LaunchService stub for handler tests. Each method
// records how it was called so assertions can check routing behavior.
type fakeLaunchService struct {
	mu sync.Mutex

	createRes LaunchResult
	createErr error
	createIDs []string

	launchRes LaunchResult
	launchErr error
	launchIDs []string

	listRes  []store.SessionRow
	listErr  error
	listOpts store.ListSessionsOptions

	getRes  map[string]*store.SessionRow
	getErr  error
	stopErr error
	stopIDs []string
	waitRes map[string]int
	waitErr error

	inputErr        error
	inputLog        [][]byte
	inputIDs        []string
	attachFn        func(ctx context.Context, id string, w io.Writer) error
	attachErr       error
	attachSinceSeqs []int64

	attachedClients map[string]int

	resizeErr  error
	resizeIDs  []string
	resizeRows []uint16
	resizeCols []uint16

	resumeRes LaunchResult
	resumeErr error
}

func (f *fakeLaunchService) CreateSession(id string) (LaunchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createIDs = append(f.createIDs, id)
	return f.createRes, f.createErr
}

func (f *fakeLaunchService) CreateSessionWithBootPrompt(id, _ string) (LaunchResult, error) {
	return f.CreateSession(id)
}

func (f *fakeLaunchService) CreateSessionWithInput(in CreateSessionInput) (LaunchResult, error) {
	return f.CreateSession(in.LaunchID)
}

func (f *fakeLaunchService) LaunchSession(id string) (LaunchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launchIDs = append(f.launchIDs, id)
	return f.launchRes, f.launchErr
}
func (f *fakeLaunchService) ListSessions(opts store.ListSessionsOptions) ([]store.SessionRow, error) {
	f.mu.Lock()
	f.listOpts = opts
	f.mu.Unlock()
	return f.listRes, f.listErr
}
func (f *fakeLaunchService) GetSession(id string) (*store.SessionRow, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	r, ok := f.getRes[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", store.ErrSessionNotFound, id)
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

func (f *fakeLaunchService) SendTurn(_ context.Context, id, text string) error {
	return f.SendInput(id, []byte(text))
}

func (f *fakeLaunchService) AttachSession(ctx context.Context, id string, w io.Writer, sinceSeq int64) error {
	f.mu.Lock()
	f.attachSinceSeqs = append(f.attachSinceSeqs, sinceSeq)
	f.mu.Unlock()
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

func (f *fakeLaunchService) ResizeSession(id string, rows, cols uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizeIDs = append(f.resizeIDs, id)
	f.resizeRows = append(f.resizeRows, rows)
	f.resizeCols = append(f.resizeCols, cols)
	return f.resizeErr
}

func (f *fakeLaunchService) ResumeLogicalAgent(_ string) (LaunchResult, error) {
	return f.resumeRes, f.resumeErr
}

func (f *fakeLaunchService) RuntimeHealth(_ string) (RuntimeHealthResult, bool) {
	return RuntimeHealthResult{}, false
}

func newTestHandler(svc LaunchService) http.Handler {
	return NewHandler(Deps{Service: svc})
}

// decodeErr pulls the typed error envelope out of a response body and
// returns it; helpers use this to assert both status + code in one place.
func decodeErr(t *testing.T, rr *httptest.ResponseRecorder) ErrorResponse {
	t.Helper()
	var env ErrorResponse
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v (raw=%q)", err, rr.Body.String())
	}
	return env
}

func TestHandleCreateSession_Success(t *testing.T) {
	svc := &fakeLaunchService{createRes: LaunchResult{SessionID: "sess-1", Workspace: "/ws/1", LogPath: "/ws/1/logs/session.log", ProviderID: "claude-stream"}}

	body, _ := json.Marshal(LaunchRequest{Launch: "demo-launch"})
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)

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
	if res.ProviderID != "claude-stream" {
		t.Errorf("provider_id = %q, want %q", res.ProviderID, "claude-stream")
	}
	if len(svc.createIDs) != 1 || svc.createIDs[0] != "demo-launch" {
		t.Errorf("CreateSession not dispatched with request body: %v", svc.createIDs)
	}
	if len(svc.launchIDs) != 0 {
		t.Errorf("LaunchSession should not fire on POST /sessions; got %v", svc.launchIDs)
	}
}

func TestHandleCreateSession_MissingID(t *testing.T) {
	svc := &fakeLaunchService{}
	body, _ := json.Marshal(LaunchRequest{})
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
	env := decodeErr(t, rr)
	if env.Error.Code != CodeInvalidRequest {
		t.Errorf("error code = %q, want %q", env.Error.Code, CodeInvalidRequest)
	}
}

func TestHandleCreateSession_ServiceError(t *testing.T) {
	svc := &fakeLaunchService{createErr: errors.New("adapter unavailable")}
	body, _ := json.Marshal(LaunchRequest{Launch: "demo"})
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
	env := decodeErr(t, rr)
	if env.Error.Code != CodeInternalError || env.Error.Message == "" {
		t.Errorf("envelope = %+v", env)
	}
}

func TestHandleLaunchSession_Success(t *testing.T) {
	svc := &fakeLaunchService{launchRes: LaunchResult{SessionID: "sess-1", Workspace: "/ws/1", LogPath: "/ws/1/logs/session.log", ProviderID: "claude-stream"}}
	req := httptest.NewRequest(http.MethodPost, "/sessions/sess-1/launch", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var res LaunchResponse
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res.ID != "sess-1" {
		t.Errorf("id = %q", res.ID)
	}
	if res.ProviderID != "claude-stream" {
		t.Errorf("provider_id = %q, want %q", res.ProviderID, "claude-stream")
	}
	if len(svc.launchIDs) != 1 || svc.launchIDs[0] != "sess-1" {
		t.Errorf("LaunchSession not dispatched: %v", svc.launchIDs)
	}
}

func TestHandleLaunchSession_NotFound(t *testing.T) {
	svc := &fakeLaunchService{launchErr: fmt.Errorf("%w: missing", store.ErrSessionNotFound)}
	req := httptest.NewRequest(http.MethodPost, "/sessions/missing/launch", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", rr.Code, rr.Body.String())
	}
	env := decodeErr(t, rr)
	if env.Error.Code != CodeNotFound {
		t.Errorf("error code = %q", env.Error.Code)
	}
}

func TestHandleLaunchSession_WrongState(t *testing.T) {
	svc := &fakeLaunchService{launchErr: fmt.Errorf("%w (state=%q)", session.ErrNotCreated, "running")}
	req := httptest.NewRequest(http.MethodPost, "/sessions/sess-1/launch", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rr.Code)
	}
	env := decodeErr(t, rr)
	if env.Error.Code != CodeConflict {
		t.Errorf("error code = %q", env.Error.Code)
	}
}

func TestHandleListSessions(t *testing.T) {
	rows := []store.SessionRow{
		{ID: "s1", State: "running", PID: sql.NullInt64{Int64: 42, Valid: true}},
		{ID: "s2", State: "completed", ExitCode: sql.NullInt64{Int64: 0, Valid: true}},
	}
	svc := &fakeLaunchService{listRes: rows}
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)

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

func TestHandleListSessions_ParamsForwarded(t *testing.T) {
	svc := &fakeLaunchService{listRes: nil}
	req := httptest.NewRequest(http.MethodGet, "/sessions?limit=25&state=running&cursor=2026-04-19T00%3A00%3A00Z", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if svc.listOpts.Limit != 25 {
		t.Errorf("Limit = %d, want 25", svc.listOpts.Limit)
	}
	if svc.listOpts.State != "running" {
		t.Errorf("State = %q, want 'running'", svc.listOpts.State)
	}
	if svc.listOpts.Cursor != "2026-04-19T00:00:00Z" {
		t.Errorf("Cursor = %q", svc.listOpts.Cursor)
	}
}

func TestHandleListSessions_NextCursor_WhenFull(t *testing.T) {
	// Page returned == limit; handler should surface last row's created_at
	// as next_cursor so the client can keep paging.
	rows := make([]store.SessionRow, 3)
	rows[0] = store.SessionRow{ID: "a", CreatedAt: "2026-04-19T10:00:00Z"}
	rows[1] = store.SessionRow{ID: "b", CreatedAt: "2026-04-19T09:00:00Z"}
	rows[2] = store.SessionRow{ID: "c", CreatedAt: "2026-04-19T08:00:00Z"}
	svc := &fakeLaunchService{listRes: rows}
	req := httptest.NewRequest(http.MethodGet, "/sessions?limit=3", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var res ListSessionsResponse
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res.NextCursor != "2026-04-19T08:00:00Z" {
		t.Errorf("NextCursor = %q, want last row's created_at", res.NextCursor)
	}
}

func TestHandleListSessions_NoNextCursor_WhenShort(t *testing.T) {
	// Fewer rows than limit → end of range, no cursor emitted.
	rows := []store.SessionRow{{ID: "a", CreatedAt: "2026-04-19T10:00:00Z"}}
	svc := &fakeLaunchService{listRes: rows}
	req := httptest.NewRequest(http.MethodGet, "/sessions?limit=10", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	var res ListSessionsResponse
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res.NextCursor != "" {
		t.Errorf("NextCursor = %q, want empty", res.NextCursor)
	}
}

func TestHandleListSessions_BadLimit(t *testing.T) {
	svc := &fakeLaunchService{}
	req := httptest.NewRequest(http.MethodGet, "/sessions?limit=bad", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
	env := decodeErr(t, rr)
	if env.Error.Code != CodeInvalidRequest {
		t.Errorf("code = %q", env.Error.Code)
	}
}

func TestHandleGetSession_NotFound(t *testing.T) {
	svc := &fakeLaunchService{getRes: map[string]*store.SessionRow{}}
	req := httptest.NewRequest(http.MethodGet, "/sessions/missing", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
	env := decodeErr(t, rr)
	if env.Error.Code != CodeNotFound {
		t.Errorf("error code = %q", env.Error.Code)
	}
}

func TestHandleGetSession_Found(t *testing.T) {
	row := &store.SessionRow{ID: "s1", State: "running"}
	svc := &fakeLaunchService{getRes: map[string]*store.SessionRow{"s1": row}}
	req := httptest.NewRequest(http.MethodGet, "/sessions/s1", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
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
	req := httptest.NewRequest(http.MethodPost, "/sessions/s1/stop", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204: %s", rr.Code, rr.Body.String())
	}
	if len(svc.stopIDs) != 1 || svc.stopIDs[0] != "s1" {
		t.Errorf("StopSession not dispatched: %v", svc.stopIDs)
	}
}

func TestHandleStopSession_NotRunning(t *testing.T) {
	svc := &fakeLaunchService{stopErr: agentsessions.ErrSessionNotRunning}
	req := httptest.NewRequest(http.MethodPost, "/sessions/s1/stop", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
	env := decodeErr(t, rr)
	if env.Error.Code != CodeNotFound {
		t.Errorf("error code = %q", env.Error.Code)
	}
}

func TestHandleWaitSession(t *testing.T) {
	svc := &fakeLaunchService{waitRes: map[string]int{"s1": 42}}
	req := httptest.NewRequest(http.MethodGet, "/sessions/s1/wait", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
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
	svc := &fakeLaunchService{waitErr: agentsessions.ErrSessionNotRunning}
	req := httptest.NewRequest(http.MethodGet, "/sessions/s1/wait", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestHandleSessions_MethodNotAllowed(t *testing.T) {
	svc := &fakeLaunchService{}

	req := httptest.NewRequest(http.MethodDelete, "/sessions", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /sessions status = %d, want 405", rr.Code)
	}
	env := decodeErr(t, rr)
	if env.Error.Code != CodeMethodNotAllowed {
		t.Errorf("error code = %q", env.Error.Code)
	}
}

func TestHandleSendInput_Success(t *testing.T) {
	svc := &fakeLaunchService{}
	req := httptest.NewRequest(http.MethodPost, "/sessions/s1/input", bytes.NewReader([]byte("hello\n")))
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
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
	svc := &fakeLaunchService{inputErr: agentsessions.ErrSessionNotRunning}
	req := httptest.NewRequest(http.MethodPost, "/sessions/s1/input", bytes.NewReader([]byte("x")))
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestHandleSendInput_NoInputChannelConflict(t *testing.T) {
	svc := &fakeLaunchService{inputErr: agentsessions.ErrNoInputChannel}
	req := httptest.NewRequest(http.MethodPost, "/sessions/s1/input", bytes.NewReader([]byte("x")))
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rr.Code)
	}
	env := decodeErr(t, rr)
	if env.Error.Code != CodeConflict {
		t.Errorf("error code = %q", env.Error.Code)
	}
}

func TestHandleSendInput_TooLarge(t *testing.T) {
	svc := &fakeLaunchService{}
	body := bytes.Repeat([]byte{'x'}, maxInputBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/sessions/s1/input", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rr.Code)
	}
	if len(svc.inputLog) != 0 {
		t.Errorf("body was forwarded despite being too large: %d entries", len(svc.inputLog))
	}
	env := decodeErr(t, rr)
	if env.Error.Code != CodePayloadTooLarge {
		t.Errorf("error code = %q", env.Error.Code)
	}
}

// ---- Resize ----

func TestHandleResize_Success(t *testing.T) {
	svc := &fakeLaunchService{}
	body := bytes.NewReader([]byte(`{"rows":42,"cols":120}`))
	req := httptest.NewRequest(http.MethodPost, "/sessions/s1/resize", body)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rr.Code)
	}
	if len(svc.resizeIDs) != 1 || svc.resizeIDs[0] != "s1" {
		t.Errorf("resize not dispatched to s1: %v", svc.resizeIDs)
	}
	if svc.resizeRows[0] != 42 || svc.resizeCols[0] != 120 {
		t.Errorf("resize dims wrong: got %dx%d, want 42x120", svc.resizeRows[0], svc.resizeCols[0])
	}
}

func TestHandleResize_ZeroRowsRejected(t *testing.T) {
	svc := &fakeLaunchService{}
	body := bytes.NewReader([]byte(`{"rows":0,"cols":120}`))
	req := httptest.NewRequest(http.MethodPost, "/sessions/s1/resize", body)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
	if len(svc.resizeIDs) != 0 {
		t.Errorf("resize was dispatched despite zero rows: %v", svc.resizeIDs)
	}
	env := decodeErr(t, rr)
	if env.Error.Code != CodeInvalidRequest {
		t.Errorf("error code = %q, want %q", env.Error.Code, CodeInvalidRequest)
	}
}

func TestHandleResize_ZeroColsRejected(t *testing.T) {
	svc := &fakeLaunchService{}
	body := bytes.NewReader([]byte(`{"rows":24,"cols":0}`))
	req := httptest.NewRequest(http.MethodPost, "/sessions/s1/resize", body)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
	env := decodeErr(t, rr)
	if env.Error.Code != CodeInvalidRequest {
		t.Errorf("error code = %q", env.Error.Code)
	}
}

func TestHandleResize_BadJSON(t *testing.T) {
	svc := &fakeLaunchService{}
	req := httptest.NewRequest(http.MethodPost, "/sessions/s1/resize", bytes.NewReader([]byte(`{bad json`)))
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
	env := decodeErr(t, rr)
	if env.Error.Code != CodeInvalidRequest {
		t.Errorf("error code = %q", env.Error.Code)
	}
}

func TestHandleResize_SessionNotRunning(t *testing.T) {
	svc := &fakeLaunchService{resizeErr: agentsessions.ErrSessionNotRunning}
	body := bytes.NewReader([]byte(`{"rows":24,"cols":80}`))
	req := httptest.NewRequest(http.MethodPost, "/sessions/s1/resize", body)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
	env := decodeErr(t, rr)
	if env.Error.Code != CodeNotFound {
		t.Errorf("error code = %q", env.Error.Code)
	}
}

func TestHandleResize_MethodNotAllowed(t *testing.T) {
	svc := &fakeLaunchService{}
	req := httptest.NewRequest(http.MethodGet, "/sessions/s1/resize", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
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
	req := httptest.NewRequest(http.MethodGet, "/sessions/s1/attach", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)

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

func TestHandleAttach_SinceSeqForwarded(t *testing.T) {
	svc := &fakeLaunchService{
		attachFn: func(ctx context.Context, id string, w io.Writer) error {
			_, _ = w.Write([]byte("tail"))
			return nil
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions/s1/attach?since_seq=42", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if len(svc.attachSinceSeqs) != 1 || svc.attachSinceSeqs[0] != 42 {
		t.Errorf("sinceSeq not forwarded: %v", svc.attachSinceSeqs)
	}
}

func TestHandleAttach_BadSinceSeq(t *testing.T) {
	svc := &fakeLaunchService{}
	req := httptest.NewRequest(http.MethodGet, "/sessions/s1/attach?since_seq=abc", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
	env := decodeErr(t, rr)
	if env.Error.Code != CodeInvalidRequest {
		t.Errorf("code = %q", env.Error.Code)
	}
}

func TestHandleAttach_NegativeSinceSeq(t *testing.T) {
	svc := &fakeLaunchService{}
	req := httptest.NewRequest(http.MethodGet, "/sessions/s1/attach?since_seq=-5", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestHandleAttach_SessionNotRunning(t *testing.T) {
	svc := &fakeLaunchService{attachErr: agentsessions.ErrSessionNotRunning}
	// With a real server we'd get 404 before streaming; our handler already
	// wrote 200 + headers, so the check is that the stream terminates cleanly
	// with no body and the handler returns.
	req := httptest.NewRequest(http.MethodGet, "/sessions/s1/attach", nil)
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { newTestHandler(svc).ServeHTTP(rr, req); close(done) }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("attach handler did not return")
	}
}

func TestSessionRoutes_NotRegisteredWithoutService(t *testing.T) {
	// Nil service means NewHandler returns a mux with no /sessions
	// routes; requests to them fall through to ServeMux's default 404.
	h := NewHandler(Deps{Service: nil})

	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 when Service is nil; got %d", rr.Code)
	}
}
