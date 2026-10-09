package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/session"
)

func TestNativeOnlyResumeRequestBindsExactSource(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"bound", `{"native_only":true,"source_session_id":"source","idempotency_key":"retry"}`, http.StatusCreated},
		{"missing_source", `{"native_only":true}`, http.StatusBadRequest},
		{"missing_mode", `{"source_session_id":"source"}`, http.StatusBadRequest},
		{"ordinary", `{}`, http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeLaunchService{resumeRes: LaunchResult{SessionID: "destination"}}
			rr := httptest.NewRecorder()
			newCheckpointTestHandler(svc, nil).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/logical-agents/agent/resume", strings.NewReader(tc.body)))
			if rr.Code != tc.want {
				t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
			}
			if tc.want == http.StatusCreated {
				if len(svc.resumeOpts) != 1 {
					t.Fatal("request not dispatched")
				}
				opts := svc.resumeOpts[0]
				if (tc.name == "bound") != opts.NativeOnly {
					t.Fatal("mode not preserved")
				}
				if tc.name == "bound" && (opts.SourceSessionID != "source" || opts.IdempotencyKey != "retry") {
					t.Fatal("source/key not preserved")
				}
			} else if len(svc.resumeOpts) != 0 {
				t.Fatal("invalid request dispatched")
			}
		})
	}
}

func TestNativeOnlyResumeUnavailableReturnsConflict(t *testing.T) {
	svc := &fakeLaunchService{resumeErr: session.ErrRecoveryConflict}
	rr := httptest.NewRecorder()
	newCheckpointTestHandler(svc, nil).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/logical-agents/agent/resume", strings.NewReader(`{"native_only":true,"source_session_id":"source"}`)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status %d", rr.Code)
	}
}
