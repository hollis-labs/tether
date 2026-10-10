package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

func remotePathRequest(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	ctx := identity.WithRemoteContext(r.Context())
	ctx = identity.WithPrincipal(ctx, identity.Principal{ID: "device-test", Kind: "device", Scopes: []string{"read", "operate", "terminal"}})
	return r.WithContext(ctx)
}

func TestRemotePathResponsesAndLocalCompatibility(t *testing.T) {
	res := LaunchResult{SessionID: "s", Workspace: "/private/worker/s", LogPath: "/private/worker/s/logs/session.log", Replayed: true}
	row := store.SessionRow{ID: "s", Workspace: res.Workspace}
	svc := &fakeLaunchService{createRes: res, launchRes: res, resumeRes: res, listRes: []store.SessionRow{row}, getRes: map[string]*store.SessionRow{"s": &row}}
	s := &Server{Service: svc}
	for _, remote := range []bool{false, true} {
		for _, tc := range []struct {
			name, method, path, body string
			handle                   func(http.ResponseWriter, *http.Request)
		}{
			{"list", "GET", "/sessions", "", s.handleListSessions},
			{"get", "GET", "/sessions/s", "", func(w http.ResponseWriter, r *http.Request) { s.handleGetSession(w, r, "s") }},
			{"create-replay", "POST", "/sessions", `{"launch":"test"}`, s.handleLaunch},
			{"start-replay", "POST", "/sessions/s/launch", "", func(w http.ResponseWriter, r *http.Request) { s.handleLaunchSession(w, r, "s") }},
			{"resume-replay", "POST", "/logical-agents/a/resume", `{}`, func(w http.ResponseWriter, r *http.Request) { s.handleResumeLogicalAgent(w, r, "a") }},
		} {
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			// Caller headers cannot select remote projection on a local socket.
			r.Header.Set("X-Tether-Remote", "true")
			if remote {
				r = remotePathRequest(tc.method, tc.path, tc.body)
			}
			w := httptest.NewRecorder()
			tc.handle(w, r)
			if w.Code != 200 {
				t.Fatalf("%s remote=%v status=%d %s", tc.name, remote, w.Code, w.Body.String())
			}
			if remote && (strings.Contains(w.Body.String(), "/private/") || !strings.Contains(w.Body.String(), sessionPathRef("workspace", "s"))) {
				t.Fatalf("%s unsafe remote response: %s", tc.name, w.Body.String())
			}
			if !remote && !strings.Contains(w.Body.String(), res.Workspace) {
				t.Fatalf("%s local path changed: %s", tc.name, w.Body.String())
			}
			if strings.HasSuffix(tc.name, "replay") {
				var dto LaunchResponse
				if err := json.Unmarshal(w.Body.Bytes(), &dto); err != nil {
					t.Fatal(err)
				}
				wantLog := res.LogPath
				if remote {
					wantLog = sessionPathRef("log", "s")
				}
				if dto.Log != wantLog || !dto.Replayed {
					t.Fatalf("log/replay lost: %+v", dto)
				}
			}
		}
	}
	if identity.IsRemote(context.Background()) {
		t.Fatal("default context became remote")
	}
}

type pathBearingRefusal struct{ error }

func (pathBearingRefusal) ErrorCode() string { return "native_fixture_refused" }

func TestRemotePathErrorsPreserveClassificationAndLocalText(t *testing.T) {
	for _, coded := range []bool{false, true} {
		err := errors.New("open /private/worker/catalog/global.yaml: unavailable")
		if coded {
			err = pathBearingRefusal{err}
		}
		svc := &fakeLaunchService{createErr: err, launchErr: err, resumeErr: err, listErr: err, getErr: err}
		s := &Server{Service: svc}
		for _, remote := range []bool{false, true} {
			for _, tc := range []struct {
				name, method, body string
				handle             func(http.ResponseWriter, *http.Request)
			}{
				{"list", "GET", "", s.handleListSessions},
				{"get", "GET", "", func(w http.ResponseWriter, r *http.Request) { s.handleGetSession(w, r, "s") }},
				{"create", "POST", `{"launch":"test"}`, s.handleLaunch},
				{"start", "POST", "", func(w http.ResponseWriter, r *http.Request) { s.handleLaunchSession(w, r, "s") }},
				{"resume", "POST", `{}`, func(w http.ResponseWriter, r *http.Request) { s.handleResumeLogicalAgent(w, r, "a") }},
			} {
				r := httptest.NewRequest(tc.method, "/fixture", strings.NewReader(tc.body))
				if remote {
					r = remotePathRequest(tc.method, "/fixture", tc.body)
				}
				w := httptest.NewRecorder()
				tc.handle(w, r)
				var out ErrorResponse
				if json.Unmarshal(w.Body.Bytes(), &out) != nil {
					t.Fatal(w.Body.String())
				}
				wantStatus, wantCode := 500, CodeInternalError
				if coded && tc.name != "list" && tc.name != "get" {
					wantStatus, wantCode = 409, "native_fixture_refused"
				}
				if w.Code != wantStatus || out.Error.Code != wantCode {
					t.Fatalf("%s classification changed: %d %+v", tc.name, w.Code, out)
				}
				if remote && (strings.Contains(out.Error.Message, "/private/") || out.Error.Message == "") {
					t.Fatalf("%s remote error exposed path: %+v", tc.name, out)
				}
				if !remote && out.Error.Message != err.Error() {
					t.Fatalf("%s local error changed: %+v", tc.name, out)
				}
			}
		}
	}
}

func TestRemotePathNativeConflictRequirements(t *testing.T) {
	err := fmt.Errorf("%w: open /private/worker/native/state: unavailable", session.ErrRecoveryConflict)
	s := &Server{Service: &fakeLaunchService{resumeErr: err}}
	for _, remote := range []bool{false, true} {
		for _, nativeOnly := range []bool{false, true} {
			body := `{}`
			if nativeOnly {
				body = `{"native_only":true,"source_session_id":"source"}`
			}
			r := httptest.NewRequest(http.MethodPost, "/logical-agents/agent/resume", strings.NewReader(body))
			if remote {
				r = remotePathRequest(http.MethodPost, r.URL.Path, body)
			}
			w := httptest.NewRecorder()
			s.handleResumeLogicalAgent(w, r, "agent")
			var out ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			want := err.Error()
			if remote {
				want = "session resume conflict"
				if nativeOnly {
					want = "native-only resume unavailable; independent current launch authority required and recorded context must be valid"
				}
			}
			if w.Code != http.StatusConflict || out.Error.Code != CodeConflict || out.Error.Message != want {
				t.Fatalf("remote=%v native=%v: %d %+v", remote, nativeOnly, w.Code, out)
			}
		}
	}
}
