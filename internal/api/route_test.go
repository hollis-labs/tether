package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/launchprofile"
)

func TestHandleCreateSessionRoute(t *testing.T) {
	svc := &fakeLaunchService{createRes: LaunchResult{SessionID: "s"}}
	r := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(`{"launch":"demo","route":{"channel":"ops","kinds":["question"]}}`))
	w := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(svc.createInputs) != 1 || svc.createInputs[0].Route == nil || svc.createInputs[0].Route.Channel != "ops" || svc.createInputs[0].Route.Kinds[0] != "question" {
		t.Fatalf("route dropped: %+v", svc.createInputs)
	}
}

func TestHandleCreateSessionInvalidRouteEnvelope(t *testing.T) {
	svc := &fakeLaunchService{createErr: fmt.Errorf("resolve: %w", &launchprofile.InvalidRouteError{Field: "kinds", Value: "bogus"})}
	w := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(`{"launch":"demo"}`)))
	var envelope ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusBadRequest || envelope.Error.Code != CodeInvalidRequest {
		t.Fatalf("status %d, envelope %+v", w.Code, envelope)
	}
}
