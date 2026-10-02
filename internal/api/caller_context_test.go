package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

type contextBindings struct{ binding registry.RuntimeBinding }

func (b contextBindings) CurrentBinding(context.Context, string) (registry.RuntimeBinding, error) {
	if b.binding.TargetURN == "" {
		return registry.RuntimeBinding{}, errors.New("unbound")
	}
	return b.binding, nil
}

func TestProxyEventAttributionOverridesVerifiedCallerClaims(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		p                      identity.Principal
		wantSession, wantClaim string
		verified               bool
	}{
		{"session", identity.Principal{ID: "session:own", Kind: "session", SessionID: "own"}, "own", "other", true},
		{"operator", identity.Principal{ID: "operator", Kind: "operator"}, "", "other", false},
		{"service", identity.Principal{ID: "service", Kind: "service"}, "", "other", false},
		{"anonymous", identity.Principal{}, "other", "other", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := &obsProxyEventStore{}
			h := NewHandler(Deps{Service: sessionsFor("own", "other"), ProxyEvents: rows})
			req := httptest.NewRequest(http.MethodPost, "/proxy/events", strings.NewReader(`{"session_id":"other","tool_name":"tool","attribution":{"verified":true,"session_id":"other"}}`))
			req.Header.Set("X-Forwarded-User-Id", "session:other")
			req.Header.Set("X-Tether-Session-Id", "other")
			if tc.p.ID != "" {
				req = req.WithContext(identity.WithPrincipal(req.Context(), tc.p))
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusCreated || len(rows.events) != 1 {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			got := rows.events[0]
			if got.Attribution.Verified != tc.verified || got.SessionID != tc.wantSession || got.ClaimedSessionID != tc.wantClaim || got.Attribution.PrincipalID != tc.p.ID {
				t.Fatalf("attribution=%+v", got)
			}
			contextReq := httptest.NewRequest(http.MethodGet, "/auth/context?session_id=other", nil).WithContext(req.Context())
			contextReq.Header.Set("X-Forwarded-User-Id", "session:other")
			contextRR := httptest.NewRecorder()
			h.ServeHTTP(contextRR, contextReq)
			var snapshot callcontext.Snapshot
			if err := json.Unmarshal(contextRR.Body.Bytes(), &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot != got.Attribution {
				t.Fatalf("endpoint=%+v row=%+v", snapshot, got.Attribution)
			}
		})
	}
}

func TestResolveCallerContextOnlyVerifiedSessionSelectsRows(t *testing.T) {
	sessions := &fakeLaunchService{getRes: map[string]*store.SessionRow{
		"own": {ID: "own", LogicalAgentID: "worker", LaunchID: "launch", ProjectID: "project", WorkstreamID: sql.NullString{String: "work", Valid: true}},
	}}
	for _, tc := range []struct {
		name           string
		p              identity.Principal
		session, agent string
	}{
		{"anonymous", identity.Principal{}, "", ""},
		{"operator", identity.Principal{ID: "operator", Kind: "operator", SessionID: "own"}, "", ""},
		{"service", identity.Principal{ID: "service", Kind: "service", SessionID: "own"}, "", ""},
		{"missing session", identity.Principal{ID: "session:absent", Kind: "session", SessionID: "absent"}, "", ""},
		{"verified own session", identity.Principal{ID: "session:own", Kind: "session", SessionID: "own"}, "own", registry.LogicalAgentBindingTarget("worker")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.p.ID != "" {
				ctx = identity.WithPrincipal(ctx, tc.p)
			}
			bindings := contextBindings{registry.RuntimeBinding{TargetURN: registry.LogicalAgentBindingTarget("worker"), SessionID: "own"}}
			got := ResolveCallerContext(ctx, sessions, bindings)
			if got.SessionID != tc.session || got.AgentURN != tc.agent || got.Verified != (tc.session != "") {
				t.Fatalf("context = %+v", got)
			}
			if got.Verified && (got.WorkstreamID != "work" || got.LaunchID != "launch" || got.ProjectID != "project") {
				t.Fatalf("missing row context: %+v", got)
			}
		})
	}
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{ID: "session:own", Kind: "session", SessionID: "own"})
	for _, binding := range []registry.RuntimeBinding{{}, {TargetURN: registry.LogicalAgentBindingTarget("worker"), SessionID: "other"}} {
		got := ResolveCallerContext(ctx, sessions, contextBindings{binding})
		if !got.Verified || got.AgentURN != "" {
			t.Fatalf("missing/stale binding invented agent: %+v", got)
		}
	}
}
