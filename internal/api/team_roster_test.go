package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/teamsvc"
)

type rosterTestOps struct {
	TeamOps
	calls   int
	request teamsvc.RosterRequest
	err     error
}

func (o *rosterTestOps) ListRoster(_ context.Context, r teamsvc.RosterRequest) (teamsvc.RosterList, error) {
	o.calls++
	o.request = r
	return teamsvc.RosterList{Runs: []teamsvc.RosterView{}}, o.err
}

func TestTeamRosterRouteSelectorsAndOptionalRegistration(t *testing.T) {
	for _, query := range []string{"caller=forged", "limit=0", "limit=101", "limit=1&limit=2", "run_id=a&after=b", "after=%zz"} {
		ops := &rosterTestOps{}
		mux := http.NewServeMux()
		(&Server{Teams: ops}).registerTeamRoutes(mux)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/teams/roster?"+query, nil))
		if w.Code != 400 || ops.calls != 0 {
			t.Fatal(query, w.Code, ops.calls)
		}
	}
	ops := &rosterTestOps{}
	mux := http.NewServeMux()
	(&Server{Teams: ops}).registerTeamRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/teams/roster?run_id=a%26b&limit=3", nil))
	if w.Code != 200 || ops.request.RunID != "a&b" || ops.request.Limit != 3 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, ops.request)
	}
	ops.err = teamsvc.ErrDenied
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/teams/roster", nil))
	if w.Code != 403 || strings.Contains(w.Body.String(), "cause") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/teams/roster", nil))
	if w.Code != 405 || w.Header().Get("Allow") != "GET" {
		t.Fatal(w.Code)
	}
	for _, ops := range []TeamOps{nil, (*rosterTestOps)(nil)} {
		mux = http.NewServeMux()
		(&Server{Teams: ops}).registerTeamRoutes(mux)
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/teams/roster", nil))
		if w.Code != 404 {
			t.Fatal("disabled route registered", w.Code)
		}
	}
}
