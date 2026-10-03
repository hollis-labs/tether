package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
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

// CW-20261003-0092: a launch for a project whose repo_root is gone is a 409 with
// its own code, naming the project and the path, on create, launch and resume. It
// is not the 500 internal_error the live daemon answered, and not a 403: nobody
// is being denied anything, the catalog entry is stale.
func TestProjectRootMissingIsAConflictWithItsOwnCode(t *testing.T) {
	refusal := fmt.Errorf("%w: %w", launch.ErrLaunchProjectRootMissing,
		&config.ProjectRootError{Project: "chrispian", Root: "/srv/u/dev/chrispian", Reason: "does not exist"})
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
			if rr.Code != http.StatusConflict {
				t.Fatalf("status = %d (%s); want 409", rr.Code, rr.Body.String())
			}
			env := decodeErr(t, rr)
			if env.Error.Code != CodeProjectRootMissing {
				t.Fatalf("code = %q; want %q", env.Error.Code, CodeProjectRootMissing)
			}
			for _, want := range []string{`"chrispian"`, "/srv/u/dev/chrispian", "does not exist"} {
				if !strings.Contains(env.Error.Message, want) {
					t.Fatalf("message %q does not mention %s", env.Error.Message, want)
				}
			}
		})
	}
}

// CW-20261003-0096: a launch refused because a project's catalog layer cannot be
// protected is a 403 with its own code, naming the project and the path, on create,
// launch and resume. Its 403 used to read "this host cannot", which sends the
// operator to bubblewrap when the fix is a catalog entry; bubblewrap missing stays
// "forbidden".
func TestProjectLayerUnprotectableIsForbiddenWithItsOwnCode(t *testing.T) {
	refusal := fmt.Errorf("%w: %w", launch.ErrProjectLayerUnprotectable,
		&config.UnprotectableLayerError{Project: "nas", Root: "/srv/u/mnt/nas/repo", Why: "runs through a symlink an agent can replace"})
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
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d (%s); want 403", rr.Code, rr.Body.String())
			}
			env := decodeErr(t, rr)
			if env.Error.Code != CodeProjectLayerUnprotectable || env.Error.Code == CodeForbidden {
				t.Fatalf("code = %q; want %q, not the generic forbidden", env.Error.Code, CodeProjectLayerUnprotectable)
			}
			for _, want := range []string{`"nas"`, "/srv/u/mnt/nas/repo", "symlink an agent can replace"} {
				if !strings.Contains(env.Error.Message, want) {
					t.Fatalf("message %q does not mention %s", env.Error.Message, want)
				}
			}
		})
	}
	// bubblewrap missing is still the generic forbidden
	rr := httptest.NewRecorder()
	newTestHandler(&fakeLaunchService{createErr: fmt.Errorf("%w: bwrap: not found", launch.ErrProtectionUnavailable)}).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(`{"launch":"l1"}`)))
	if env := decodeErr(t, rr); rr.Code != http.StatusForbidden || env.Error.Code != CodeForbidden {
		t.Fatalf("protection unavailable: status %d code %q; want 403 forbidden", rr.Code, env.Error.Code)
	}
}
