package app

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/store"
)

// Exercise HTTP parsing and the real create/launch service, not fabricated rows.
type hubRelaunchAPI struct {
	api.LaunchService
	svc *Service
}

func (a hubRelaunchAPI) CreateSessionWithInput(in api.CreateSessionInput) (api.LaunchResult, error) {
	l, err := a.svc.CreateSessionWithInput(CreateSessionInput{LaunchID: in.LaunchID, Continuity: in.Continuity, IdempotencyPrincipal: in.IdempotencyPrincipal})
	if err != nil {
		return api.LaunchResult{}, err
	}
	return api.LaunchResult{SessionID: l.SessionID}, nil
}
func (a hubRelaunchAPI) LaunchSession(id string) (api.LaunchResult, error) {
	l, err := a.svc.LaunchSession(id)
	if err != nil {
		return api.LaunchResult{}, err
	}
	return api.LaunchResult{SessionID: l.SessionID}, nil
}
func TestHTTPExplicitHubRelaunchFromOrphanPersistsLineage(t *testing.T) {
	svc := relaunchHarness(t)
	h := api.NewHandler(api.Deps{Service: hubRelaunchAPI{svc: svc}})
	call := func(path, body string) string {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(body)))
		if w.Code != 200 && w.Code != 201 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var out struct {
			SessionID string `json:"id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.SessionID
	}
	first := call("/sessions", `{"launch":"l1"}`)
	parent := first
	for range 2 {
		if err := svc.Store.UpdateSessionState(parent, "orphaned", 0, nil); err != nil {
			t.Fatal(err)
		}
		child := call("/sessions", `{"launch":"l1","continuity":"relaunch"}`)
		if p, e := svc.Store.GetHubResumeLineage(context.Background(), child); e != nil || p != nil {
			t.Fatalf("unlaunched proof: %+v %v", p, e)
		}
		call("/sessions/"+child+"/launch", `{}`)
		t.Cleanup(func() { _ = svc.Manager.Stop(context.Background(), child) })
		// Saved-plan decoding proves the in-memory-only continuity flag is gone.
		p, e := svc.Store.GetLaunchPlan(child)
		if e != nil {
			t.Fatal(e)
		}
		if p.Continuity != "" || p.ResumeSourceSessionID != "" {
			t.Fatalf("unexpected serialized intent: %+v", p)
		}
		proof, e := svc.Store.GetHubResumeLineage(context.Background(), child)
		if e != nil || proof == nil || proof.SourceSessionID != parent {
			t.Fatalf("HTTP relaunch proof: %+v %v", proof, e)
		}
		parent = child
	}
	rows, e := svc.Store.ListSessions(store.ListSessionsOptions{Limit: 10})
	if e != nil {
		t.Fatal(e)
	}
	for _, row := range rows {
		if row.ID == first {
			continue
		}
		p, e := svc.Store.GetHubResumeLineage(context.Background(), row.ID)
		if e != nil || p == nil {
			t.Fatalf("historical proof lost: %+v %v", p, e)
		}
	}
}
