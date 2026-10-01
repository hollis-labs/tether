package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
)

// CW-20261001-0130: a launch refused because its agent names an undefined
// sandbox profile is a 404 not_found naming the profile, on create, launch
// and resume -- not a 500.
func TestUnknownSandboxProfileIsNotFound(t *testing.T) {
	refusal := fmt.Errorf("launch %q: %w: agent %q names sandbox profile %q, which is not defined under sandbox-profiles/",
		"l1", config.ErrUnknownSandboxProfile, "agent-1", "no-such-profile")
	for _, tc := range []struct {
		name   string
		svc    *fakeLaunchService
		method string
		path   string
		body   string
	}{
		{"create", &fakeLaunchService{createErr: refusal}, http.MethodPost, "/sessions", `{"launch":"l1"}`},
		{"launch", &fakeLaunchService{launchErr: refusal}, http.MethodPost, "/sessions/s1/launch", ``},
		{"resume", &fakeLaunchService{resumeErr: refusal}, http.MethodPost, "/logical-agents/agent-1/resume", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			newTestHandler(tc.svc).ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(tc.body)))
			if rr.Code != http.StatusNotFound {
				t.Fatalf("status = %d (%s); want 404", rr.Code, rr.Body.String())
			}
			var env ErrorResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
				t.Fatal(err)
			}
			if env.Error.Code != CodeNotFound || !strings.Contains(env.Error.Message, "no-such-profile") {
				t.Fatalf("error = %+v; want not_found naming the profile", env.Error)
			}
		})
	}
}
