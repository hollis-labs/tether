package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/execpolicy"
)

type execLaunchService struct {
	*fakeLaunchService
	st execpolicy.Status
}

func (e *execLaunchService) ExecutionStatus() (execpolicy.Status, error) { return e.st, nil }
func (e *execLaunchService) SetExecutionPolicy(p execpolicy.Policy) (execpolicy.Status, error) {
	e.st.Policy = p
	return e.st, nil
}

func (e *execLaunchService) Capacity() (execpolicy.CapacityView, error) {
	return execpolicy.CapacityView{EnvironmentID: "local", MaxSessions: 2, ActiveSessions: 2, Admission: "full"}, nil
}

func TestCapacityRoute(t *testing.T) {
	h := newTestHandler(&execLaunchService{fakeLaunchService: &fakeLaunchService{}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/capacity", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"admission":"full"`) {
		t.Fatalf("GET /capacity = %d %s", rec.Code, rec.Body)
	}
}

func TestExecutionPolicyRoutes(t *testing.T) {
	svc := &execLaunchService{fakeLaunchService: &fakeLaunchService{}}
	h := newTestHandler(svc)
	do := func(method, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/execution/policy", strings.NewReader(body)))
		return rec
	}
	if rec := do(http.MethodGet, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"paused":false`) {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body)
	}
	if rec := do(http.MethodPut, `{"paused":true,"max_sessions":2}`); rec.Code != 200 || !svc.st.Policy.Paused || svc.st.Policy.MaxSessions != 2 {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	if rec := do(http.MethodPut, `{"max_sessions":-1}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), CodeInvalidRequest) {
		t.Fatalf("negative cap = %d %s", rec.Code, rec.Body)
	}
	if rec := do(http.MethodPut, `{"nope":1}`); rec.Code != 400 {
		t.Fatalf("unknown field = %d", rec.Code)
	}
	if rec := do(http.MethodDelete, ""); rec.Code != 405 {
		t.Fatalf("DELETE = %d", rec.Code)
	}
}

func TestLaunchRefusedByExecutionPolicyIs409(t *testing.T) {
	svc := &fakeLaunchService{launchErr: &execpolicy.Refusal{Code: execpolicy.CodePaused, Message: "execution is paused"}}
	rec := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/s1/launch", nil))
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), execpolicy.CodePaused) {
		t.Fatalf("launch = %d %s", rec.Code, rec.Body)
	}
}
