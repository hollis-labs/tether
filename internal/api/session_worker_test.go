package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/workerhub"
)

func postSession(t *testing.T, svc *fakeLaunchService, cat *fakeCatalogLoader, req LaunchRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(req)
	rr := httptest.NewRecorder()
	NewHandler(Deps{Service: svc, Catalog: cat}).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewReader(body)))
	return rr
}

func TestHandleCreateSession_DefaultsToLaunchWorker(t *testing.T) {
	cat := &fakeCatalogLoader{cat: &config.Catalog{Launches: map[string]config.Launch{"pm": {Worker: "agent-os"}}}}
	svc := &fakeLaunchService{createRes: LaunchResult{SessionID: "s1"}}
	if rr := postSession(t, svc, cat, LaunchRequest{Launch: "pm"}); rr.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	if len(svc.createInputs) != 1 || svc.createInputs[0].Worker != "agent-os" {
		t.Fatalf("inputs = %+v, want worker agent-os", svc.createInputs)
	}

	// An explicit worker wins over the launch default.
	svc = &fakeLaunchService{createRes: LaunchResult{SessionID: "s2"}}
	postSession(t, svc, cat, LaunchRequest{Launch: "pm", Worker: "other"})
	if len(svc.createInputs) != 1 || svc.createInputs[0].Worker != "other" {
		t.Fatalf("inputs = %+v, want worker other", svc.createInputs)
	}

	// A launch without a worker stays on the plain path.
	cat = &fakeCatalogLoader{cat: &config.Catalog{Launches: map[string]config.Launch{"pm": {}}}}
	svc = &fakeLaunchService{createRes: LaunchResult{SessionID: "s3"}}
	postSession(t, svc, cat, LaunchRequest{Launch: "pm"})
	if len(svc.createInputs) != 0 || len(svc.createIDs) != 1 {
		t.Fatalf("inputs=%+v ids=%v, want plain CreateSession", svc.createInputs, svc.createIDs)
	}
}

func TestHandleCreateSession_WorkerUnavailable(t *testing.T) {
	for name, werr := range map[string]error{
		"not enrolled": workerhub.ErrNotEnrolled,
		"offline":      workerhub.ErrOffline,
		"no channel":   workerhub.ErrChannelUnavailable,
	} {
		t.Run(name, func(t *testing.T) {
			svc := &fakeLaunchService{createErr: fmt.Errorf("worker %q: %w", "w1", werr)}
			rr := postSession(t, svc, nil, LaunchRequest{Launch: "pm", Worker: "w1"})
			if rr.Code != http.StatusBadGateway {
				t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
			}
			if env := decodeErr(t, rr); env.Error.Code != CodeWorkerUnavailable {
				t.Fatalf("code = %q, want %q", env.Error.Code, CodeWorkerUnavailable)
			}
		})
	}
}
