package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

func TestListRecoverySessionStates(t *testing.T) {
	for _, state := range []string{"detached", "orphaned"} {
		t.Run(state, func(t *testing.T) {
			svc := &fakeLaunchService{listRes: []store.SessionRow{{ID: "s", State: state}}}
			rr := httptest.NewRecorder()
			NewHandler(Deps{Service: svc}).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sessions?state="+state, nil))
			if rr.Code != http.StatusOK || svc.listOpts.State != state {
				t.Fatalf("list = %d %s, opts %+v", rr.Code, rr.Body, svc.listOpts)
			}
			var response ListSessionsResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if len(response.Sessions) != 1 || response.Sessions[0].State != state {
				t.Fatalf("response: %+v", response)
			}
		})
	}
}

func TestResumeDetachedReturnsTypedConflict(t *testing.T) {
	svc := &fakeLaunchService{resumeErr: fmt.Errorf("resume: %w", session.ErrDetached)}
	rr := httptest.NewRecorder()
	newCheckpointTestHandler(svc, &fakeCheckpoints{}).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/logical-agents/worker/resume", nil))
	if rr.Code != http.StatusConflict || decodeErr(t, rr).Error.Code != CodeConflict {
		t.Fatalf("response: %d %s", rr.Code, rr.Body)
	}
}
