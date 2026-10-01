package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
)

// CW-20261001-0142: a launch refused because its agent would work inside a
// write-protected directory, because it is an ACP launch the ACP launcher
// cannot protect yet, or because protection is on and bubblewrap is
// missing, is a 403 forbidden carrying the reason, on create, launch and
// resume -- not a 500.
func TestProtectedPathRefusalsAreForbidden(t *testing.T) {
	inside := fmt.Errorf("%w: the agent's work directory /c/p is inside /c, which Tether write-protects for every agent; move it out of that directory", launch.ErrLaunchInsideProtectedPath)
	acp := fmt.Errorf("build runtime: %w", launch.ErrACPLaunchUnprotected)
	noBwrap := fmt.Errorf("%w: bwrap not found: install bubblewrap, or set TETHER_SANDBOX_PROTECT=0 in tetherd's environment to run agents unprotected", launch.ErrProtectionUnavailable)
	for _, refusal := range []struct {
		err  error
		want string
	}{
		{inside, "inside /c"},
		{acp, "CW-20261001-0162"},
		{noBwrap, "install bubblewrap"},
	} {
		for _, tc := range []struct {
			name   string
			svc    *fakeLaunchService
			method string
			path   string
			body   string
		}{
			{"create", &fakeLaunchService{createErr: refusal.err}, http.MethodPost, "/sessions", `{"launch":"l1"}`},
			{"launch", &fakeLaunchService{launchErr: refusal.err}, http.MethodPost, "/sessions/s1/launch", ``},
			{"resume", &fakeLaunchService{resumeErr: refusal.err}, http.MethodPost, "/logical-agents/agent-1/resume", ``},
		} {
			t.Run(tc.name+"/"+refusal.want, func(t *testing.T) {
				rr := httptest.NewRecorder()
				newTestHandler(tc.svc).ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(tc.body)))
				if rr.Code != http.StatusForbidden {
					t.Fatalf("status = %d (%s); want 403", rr.Code, rr.Body.String())
				}
				env := decodeErr(t, rr)
				if env.Error.Code != CodeForbidden || !strings.Contains(env.Error.Message, refusal.want) {
					t.Fatalf("error = %+v; want forbidden containing %q", env.Error, refusal.want)
				}
			})
		}
	}
}

// A turn Tether refuses on a codex session left to codex's own sandbox, because
// that sandbox may have been widened since the launch, is a 403 forbidden
// naming the file and how to recover, on both /input and /turn, not a 500
// (CW-20261001-0142).
func TestSendInputAndTurn_SandboxWidenedIsForbidden(t *testing.T) {
	refusal := fmt.Errorf("%w: /p/.codex/config.toml exists; remove it, or relaunch the session so Tether wraps the agent", launch.ErrCodexSandboxWidened)
	for _, tc := range []struct{ name, path, body string }{
		{"input", "/sessions/s1/input", "hello"},
		{"turn", "/sessions/s1/turn", `{"text":"hello"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			newTestHandler(&fakeLaunchService{inputErr: refusal}).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, tc.path, bytes.NewBufferString(tc.body)))
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d (%s); want 403", rr.Code, rr.Body.String())
			}
			env := decodeErr(t, rr)
			if env.Error.Code != CodeForbidden || !strings.Contains(env.Error.Message, ".codex/config.toml") || !strings.Contains(env.Error.Message, "relaunch") {
				t.Fatalf("error = %+v; want forbidden naming the file and the recovery", env.Error)
			}
		})
	}
}
