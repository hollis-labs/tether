package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/agentlive"
)

type fakeAgentsLive struct {
	LaunchService
	q     agentlive.Query
	watch *agentlive.Tracker
}

func (f *fakeAgentsLive) AgentsLive(_ context.Context, q agentlive.Query, w *agentlive.Tracker) (agentlive.Response, error) {
	f.q, f.watch = q, w
	return agentlive.Response{Agents: []agentlive.Row{}, ObservedAt: time.Unix(0, 0).UTC(), StaleAfterSeconds: int(q.StaleAfter.Seconds())}, nil
}

func TestAgentsLiveRoute(t *testing.T) {
	f := &fakeAgentsLive{}
	h := NewHandler(Deps{Service: f})
	get := func(target string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		return w
	}
	w := get("/agents/live?host=sol&kind=kit_seat&include_stale=true&stale_after_seconds=30&limit=5")
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if f.q.Host != "sol" || f.q.Kind != "kit_seat" || !f.q.IncludeStale || f.q.StaleAfter != 30*time.Second || f.q.Limit != 5 || f.watch == nil {
		t.Errorf("query not applied: %+v", f.q)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out["agents"] == nil || out["stale_after_seconds"].(float64) != 30 {
		t.Errorf("envelope: %s", w.Body)
	}
	for _, bad := range []string{"kind=nope", "include_stale=maybe", "stale_after_seconds=0x", "stale_after_seconds=-5", "stale_after_seconds=999999", "limit=abc", "limit=5000", "cursor=zz", "cursor=o-3"} {
		if w := get("/agents/live?" + bad); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/agents/live", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", w.Code)
	}
}

func TestAgentsLiveRequiresReadScope(t *testing.T) {
	for _, r := range RemoteRouteScopes {
		if r.Path == "/agents/live" {
			if r.Methods["GET"] != readScope || len(r.Methods) != 1 {
				t.Fatalf("scope: %+v", r)
			}
			return
		}
	}
	t.Fatal("no remote scope for /agents/live")
}
