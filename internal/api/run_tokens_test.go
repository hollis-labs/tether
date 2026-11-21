package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
)

type runRevokerFixture struct {
	LaunchService
	ids []string
}

func (f *runRevokerFixture) RevokeRunToken(_ context.Context, id string) error {
	f.ids = append(f.ids, id)
	return nil
}

func TestRunRevokeRequiresLocalOperatorAndExactIdentifier(t *testing.T) {
	f := &runRevokerFixture{}
	mux := http.NewServeMux()
	(&Server{Service: f}).registerRunTokenRoutes(mux)
	narrow := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}, TokenID: "issued-jti", Audience: "tether"}
	for _, tc := range []struct {
		p     *identity.Principal
		local bool
		body  string
		want  int
	}{
		{nil, true, `{"target":"run-id"}`, 401},
		{&opP, false, `{"target":"run-id"}`, 403},
		{&narrow, true, `{"target":"run-id"}`, 403},
		{&opP, true, `{"target":"not.a.token"}`, 400},
		{&opP, true, `{}`, 400},
		{&opP, true, `{"target":"run-id"}`, 200},
	} {
		req := httptest.NewRequest(http.MethodPost, RunTokenRevokePath, strings.NewReader(tc.body))
		ctx := req.Context()
		if tc.local {
			ctx = localCtx(t)
		}
		if tc.p != nil {
			ctx = identity.WithPrincipal(ctx, *tc.p)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req.WithContext(ctx))
		if w.Code != tc.want {
			t.Fatalf("status %d, want %d", w.Code, tc.want)
		}
	}
	if len(f.ids) != 1 || f.ids[0] != "run-id" {
		t.Fatal("refused request reached revoker")
	}
}
