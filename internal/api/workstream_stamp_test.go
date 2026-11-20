package api

import (
	"context"
	"database/sql"
	"github.com/hollis-labs/tether/internal/store"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
)

func TestWorkstreamStampPolicy(t *testing.T) {
	op := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator"}
	for _, tc := range []struct {
		name   string
		p      identity.Principal
		id     string
		status int
		want   string
	}{
		{"ULID", op, "01M7VQ2QWYB5RZR86W2BWG8PW3", 201, "01M7VQ2QWYB5RZR86W2BWG8PW3"},
		{"UUIDv7", op, "019a9e45-6789-7123-8123-0123456789ab", 201, "019a9e45-6789-7123-8123-0123456789ab"},
		{"opaque", op, "external.work-1_ref", 201, "external.work-1_ref"},
		{"omitted", op, "", 201, ""},
		{"whitespace", op, " ", 400, ""},
		{"shell", op, "a;echo", 400, ""},
		{"path", op, "a/b", 400, ""},
		{"too long", op, strings.Repeat("a", 65), 400, ""},
		{"bounded", op, strings.Repeat("a", 64), 201, strings.Repeat("a", 64)},
		{"agent", identity.Principal{Verified: true, ID: "agent:a", Kind: "agent"}, "work", 403, ""},
		{"session", identity.Principal{Verified: true, ID: "session:s", Kind: "session", SessionID: "s"}, "work", 403, ""},
		{"operator session claim", identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", SessionID: "s"}, "work", 403, ""},
		{"narrow operator", identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", TokenID: "jti"}, "work", 201, ""},
		{"unverified", identity.Principal{ID: identity.OperatorID, Kind: "operator"}, "work", 201, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeLaunchService{}
			h := NewHandler(Deps{Service: svc})
			r := httptest.NewRequest("POST", "/sessions", strings.NewReader(`{"launch":"l1","workstream_id":"`+tc.id+`"}`)).WithContext(identity.WithPrincipal(context.Background(), tc.p))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if tc.status == 201 {
				if len(svc.createInputs) != 1 || svc.createInputs[0].WorkstreamID != tc.want {
					t.Fatalf("input: %+v", svc.createInputs)
				}
			} else if len(svc.createInputs) != 0 {
				t.Fatal("refused stamp reached create")
			}
		})
	}
}

func TestSessionLaunchCorrelationAssertions(t *testing.T) {
	op := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator"}
	for _, tc := range []struct {
		name, body string
		p          identity.Principal
		status     int
	}{
		{"legacy", "", op, 200},
		{"matching", `{"task_id":"CW-1","workstream_id":"external"}`, op, 200},
		{"mismatch", `{"workstream_id":"other"}`, op, 409},
		{"invalid", `{"workstream_id":"a/b"}`, op, 400},
		{"agent", `{"workstream_id":"external"}`, identity.Principal{Verified: true, ID: "agent:a", Kind: "agent"}, 403},
		{"malformed", `{`, op, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeLaunchService{getRes: map[string]*store.SessionRow{"s1": {ID: "s1", TaskID: sql.NullString{String: "CW-1", Valid: true}, WorkstreamID: sql.NullString{String: "external", Valid: true}}}}
			r := httptest.NewRequest("POST", "/sessions/s1/launch", strings.NewReader(tc.body)).WithContext(identity.WithPrincipal(context.Background(), tc.p))
			w := httptest.NewRecorder()
			NewHandler(Deps{Service: svc}).ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if tc.status != 200 && len(svc.launchIDs) != 0 {
				t.Fatal("refused assertion launched session")
			}
		})
	}
}
