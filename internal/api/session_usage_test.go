package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

type fakeSessionUsage struct{ u store.SessionUsage }

func (f fakeSessionUsage) GetSessionUsage(id string) (store.SessionUsage, error) {
	u := f.u
	u.SessionID = id
	return u, nil
}

func TestHandleSessionUsage(t *testing.T) {
	svc := &fakeLaunchService{getRes: map[string]*store.SessionRow{"s1": {ID: "s1", State: "running"}}}
	for name, tc := range map[string]struct {
		u       store.SessionUsage
		metered bool
	}{
		"unmetered": {store.SessionUsage{Turns: 3}, false},
		"metered":   {store.SessionUsage{Turns: 3, UsageTurns: 2, InputTokens: 9}, true},
	} {
		t.Run(name, func(t *testing.T) {
			h := NewHandler(Deps{Service: svc, SessionUsage: fakeSessionUsage{tc.u}})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sessions/s1/usage", nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
			}
			var got SessionUsageResponse
			if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got.SessionID != "s1" || got.Turns != 3 || got.Metered != tc.metered {
				t.Fatalf("got %+v", got)
			}
		})
	}
	rr := httptest.NewRecorder()
	NewHandler(Deps{Service: svc, SessionUsage: fakeSessionUsage{}}).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sessions/nope/usage", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing session status %d", rr.Code)
	}
}
