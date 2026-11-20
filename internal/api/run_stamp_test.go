package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
)

func TestRunStampLauncherPolicyAndCreationOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		p      *identity.Principal
		status int
		stored bool
	}{
		{"operator", &identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator"}, http.StatusOK, true},
		{"agent", &identity.Principal{Verified: true, ID: "agent:a", Kind: "agent"}, http.StatusForbidden, false},
		{"session", &identity.Principal{Verified: true, ID: "session:s", Kind: "session", SessionID: "s"}, http.StatusForbidden, false},
		{"unverified operator", &identity.Principal{ID: identity.OperatorID, Kind: "operator"}, http.StatusOK, false},
		{"device", &identity.Principal{Verified: true, ID: "msg://device/w", Kind: "device"}, http.StatusOK, false},
		{"narrow token", &identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", TokenID: "jti"}, http.StatusOK, false},
		{"anonymous", nil, http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, db := bootstrapAs(t, tc.p)
			if code := bootstrap(t, srv.URL, map[string]any{"session_id": "s1", "run_id": "42"}); code != tc.status {
				t.Fatalf("status=%d want=%d", code, tc.status)
			}
			row, err := db.GetSession("s1")
			if tc.status != http.StatusOK {
				if err == nil {
					t.Fatal("refused caller created session")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if row.RunID.Valid != tc.stored || (tc.stored && row.RunID.String != "42") {
				t.Fatalf("run stamp=%+v", row.RunID)
			}
			if tc.stored {
				if code := bootstrap(t, srv.URL, map[string]any{"session_id": "s1", "run_id": "43"}); code != http.StatusOK {
					t.Fatal(code)
				}
				row, err = db.GetSession("s1")
				if err != nil || row.RunID.String != "42" {
					t.Fatal("bootstrap rebound existing run")
				}
				if code := bootstrap(t, srv.URL, map[string]any{"session_id": "bad", "run_id": "run with spaces"}); code != http.StatusBadRequest {
					t.Fatalf("malformed run accepted: %d", code)
				}
			}
		})
	}
}

func TestLaunchRunStampPolicy(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		status int
		want   string
	}{{"operator", 201, "42"}, {"agent", 403, ""}, {"device", 201, ""}} {
		t.Run(tc.kind, func(t *testing.T) {
			svc := &fakeLaunchService{}
			h := NewHandler(Deps{Service: svc})
			principal := identity.Principal{Verified: true, ID: "msg://device/w", Kind: tc.kind}
			if tc.kind == "operator" {
				principal.ID = identity.OperatorID
			}
			ctx := identity.WithPrincipal(context.Background(), principal)
			req := httptest.NewRequest("POST", "/sessions", strings.NewReader(`{"launch":"l1","run_id":"42"}`)).WithContext(ctx)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if tc.status == 403 {
				if len(svc.createInputs) != 0 {
					t.Fatal("denied run reached service")
				}
				return
			}
			if len(svc.createInputs) != 1 || svc.createInputs[0].RunID != tc.want {
				t.Fatalf("run input=%+v", svc.createInputs)
			}
		})
	}
}
