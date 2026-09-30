package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

func postIdem(t *testing.T, svc LaunchService, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	return rr
}

// A keyed create reaches the service with its key, and a replay answers 200
// with replayed=true where a fresh create answers 201.
func TestHandleLaunch_IdempotencyKey(t *testing.T) {
	for _, tc := range []struct {
		name     string
		replayed bool
		status   int
	}{{"fresh", false, http.StatusCreated}, {"replay", true, http.StatusOK}} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeLaunchService{createRes: LaunchResult{SessionID: "sess-1", Replayed: tc.replayed}}
			rr := postIdem(t, svc, "/sessions", `{"launch":"demo","idempotency_key":"test/k1"}`)
			if rr.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rr.Code, tc.status, rr.Body)
			}
			if len(svc.createInputs) != 1 || svc.createInputs[0].IdempotencyKey != "test/k1" {
				t.Fatalf("service inputs = %+v; want the key passed through", svc.createInputs)
			}
			var resp LaunchResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Replayed != tc.replayed || resp.ID != "sess-1" {
				t.Errorf("response = %+v", resp)
			}
			if !strings.Contains(rr.Body.String(), `"replayed":`) {
				t.Errorf("replayed is not on the wire: %s", rr.Body)
			}
		})
	}
}

// A key reused with a different request answers 409 idempotency_conflict,
// on create and on resume alike.
func TestIdempotencyConflictIsTyped(t *testing.T) {
	conflict := fmt.Errorf("%w (key bound to session s1 by a create request)", store.ErrIdempotencyConflict)
	for _, tc := range []struct{ path, body string }{
		{"/sessions", `{"launch":"demo","idempotency_key":"test/k1"}`},
		{"/logical-agents/agent/resume", `{"idempotency_key":"test/k1"}`},
	} {
		svc := &fakeLaunchService{createErr: conflict, resumeErr: conflict}
		rr := postIdem(t, svc, tc.path, tc.body)
		if rr.Code != http.StatusConflict {
			t.Errorf("%s: status = %d, want 409", tc.path, rr.Code)
			continue
		}
		if env := decodeErr(t, rr); env.Error.Code != CodeIdempotencyConflict {
			t.Errorf("%s: error = %+v", tc.path, env.Error)
		}
	}
}

func TestHandleLaunch_RejectsMalformedKeys(t *testing.T) {
	for name, key := range map[string]string{
		"padded":   " test/k1",
		"control":  "test/\u0007",
		"too long": strings.Repeat("k", maxIdempotencyKeyBytes+1),
	} {
		svc := &fakeLaunchService{}
		body, _ := json.Marshal(map[string]string{"launch": "demo", "idempotency_key": key})
		rr := postIdem(t, svc, "/sessions", string(body))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rr.Code)
		}
		if len(svc.createIDs) != 0 {
			t.Errorf("%s: a malformed key reached the service", name)
		}
	}
}

// Resume keeps accepting an empty body, and passes a key from a JSON one.
func TestHandleResume_OptionalIdempotencyBody(t *testing.T) {
	svc := &fakeLaunchService{resumeRes: LaunchResult{SessionID: "sess-r"}}
	if rr := postIdem(t, svc, "/logical-agents/agent/resume", ""); rr.Code != http.StatusCreated {
		t.Fatalf("empty body: status = %d: %s", rr.Code, rr.Body)
	}
	svc.resumeRes.Replayed = true
	rr := postIdem(t, svc, "/logical-agents/agent/resume", `{"idempotency_key":"test/r1"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("keyed replay: status = %d: %s", rr.Code, rr.Body)
	}
	if len(svc.resumeOpts) != 2 || svc.resumeOpts[0].IdempotencyKey != "" || svc.resumeOpts[1].IdempotencyKey != "test/r1" {
		t.Fatalf("resume options = %+v", svc.resumeOpts)
	}
}
