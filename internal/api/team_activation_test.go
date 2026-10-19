package api

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/catalogapply"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/teamactivation"
)

type activationTestService struct {
	LaunchService
	calls int
	query teamactivation.Query
}

func (s *activationTestService) InspectTeamActivation(_ context.Context, q teamactivation.Query) (teamactivation.Evidence, error) {
	s.calls++
	s.query = q
	private := teamactivation.Launch{EffectiveSandboxPolicyDigest: "PRIVATE_EFFECTIVE", InstalledSandboxFileDigest: "PRIVATE_INSTALLED", InstalledSandboxPolicyDigest: "PRIVATE_PARSED"}
	return teamactivation.Evidence{Mapping: "legacy_catalog", Launch: private, LoadedBaseline: &private, Admitted: &private, Created: &private, Launched: &private, EvidenceDigest: "PRIVATE_AGGREGATE"}, nil
}

func TestTeamActivationRouteSelectorsAuthorityAndNoStore(t *testing.T) {
	svc := &activationTestService{}
	mux := http.NewServeMux()
	(&Server{Service: svc}).registerTeamActivationRoute(mux)
	for _, q := range []string{"", "launch_id=a&launch_id=b", "launch_id=a&pin=forged", "launch_id=%zz"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/teams/activation?"+q, nil))
		if w.Code != 400 || svc.calls != 0 {
			t.Fatal("invalid selector reached inspector", q, w.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/teams/activation?launch_id=a&session_id=s", nil)
	req = req.WithContext(identity.WithPrincipal(req.Context(), identity.Principal{Kind: "session", Scopes: []string{"message.write"}}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 403 || svc.calls != 0 {
		t.Fatal("unprivileged inspection accepted")
	}
	req = req.WithContext(identity.WithPrincipal(req.Context(), identity.Principal{Kind: "session", Scopes: []string{"tether.read"}}))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || svc.query.SessionID != "s" {
		t.Fatal("inspection contract", w.Code, svc.query)
	}
	if strings.Contains(w.Body.String(), "PRIVATE_") {
		t.Fatal("general reader received selector-sensitive digest")
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/teams/activation", nil))
	if w.Code != 405 {
		t.Fatal(w.Code)
	}
}

func TestTeamActivationSensitiveProjectionRequiresExistingOperatorGate(t *testing.T) {
	svc := &activationTestService{}
	mux := http.NewServeMux()
	(&Server{Service: svc}).registerTeamActivationRoute(mux)
	req := httptest.NewRequest(http.MethodGet, "/teams/activation?session_id=s", nil)
	ctx := identity.ConnectionContext(req.Context(), &net.UnixConn{})
	ctx = identity.WithPrincipal(ctx, identity.Principal{Kind: "operator", ID: identity.OperatorID, Verified: true})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req.WithContext(ctx))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "PRIVATE_EFFECTIVE") {
		t.Fatal("operator projection unavailable")
	}
	req = httptest.NewRequest(http.MethodGet, "/teams/activation?session_id=s", nil)
	req.Header.Set("X-Local-Operator", "true")
	req = req.WithContext(identity.WithPrincipal(req.Context(), identity.Principal{Kind: "session", Scopes: []string{"tether.read"}}))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 || strings.Contains(w.Body.String(), "PRIVATE_") {
		t.Fatal("request header bypassed operator digest gate")
	}
}

type activationPlanTestService struct {
	CatalogApplyService
	calls int
}

func (s *activationPlanTestService) PlanTeamActivation(context.Context, catalogapply.TeamActivationRequest, catalogapply.Options) (*catalogapply.Plan, error) {
	s.calls++
	return &catalogapply.Plan{State: catalogapply.StatePlanned}, nil
}

func TestTeamActivationRoutesThroughLocalAndRemoteScopeWrappers(t *testing.T) {
	for _, tc := range []struct {
		name, method, path string
		remote             bool
		principal          identity.Principal
		want               int
	}{
		{"remote structural reader", "GET", "/teams/activation?launch_id=a", true, identity.Principal{ID: "device", Kind: "device", Scopes: []string{"read"}}, 200},
		{"remote missing read", "GET", "/teams/activation?launch_id=a", true, identity.Principal{ID: "device", Kind: "device", Scopes: []string{"operate"}}, 403},
		{"remote cannot plan", "POST", "/catalog/plans/team-activation", true, identity.Principal{ID: "device", Kind: "device", Scopes: []string{"read", "admin", "maintain", "catalog.write"}}, 403},
		{"local issued structural reader", "GET", "/teams/activation?launch_id=a", false, identity.Principal{ID: "session", Kind: "agent", TokenID: "issued", Scopes: []string{"tether.read"}}, 200},
		{"local issued missing read", "GET", "/teams/activation?launch_id=a", false, identity.Principal{ID: "session", Kind: "agent", TokenID: "issued", Scopes: []string{"tether.operate"}}, 403},
		{"local issued cannot borrow custody", "POST", "/catalog/plans/team-activation", false, identity.Principal{ID: identity.OperatorID, Kind: "operator", TokenID: "issued", Scopes: []string{"*", "catalog.write"}}, 403},
		{"local stored session missing planning scope", "POST", "/catalog/plans/team-activation", false, identity.Principal{ID: "session", Kind: "session", Scopes: []string{"tether.read"}}, 403},
		{"existing local planning scope", "POST", "/catalog/plans/team-activation", false, identity.Principal{ID: "session", Kind: "session", Scopes: []string{"catalog.write"}}, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &activationTestService{}
			plans := &activationPlanTestService{}
			mux := http.NewServeMux()
			s := &Server{Service: svc, CatalogApply: plans}
			s.registerTeamActivationRoute(mux)
			s.registerCatalogApplyRoutes(mux)
			wrapped := RemoteScopeMiddleware(AgentScopeMiddleware(identity.Enforce, mux))
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
			ctx := r.Context()
			p := tc.principal
			p.Verified = true
			ctx = identity.WithPrincipal(ctx, p)
			if tc.remote {
				ctx = identity.WithRemoteContext(ctx)
			}
			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, r.WithContext(ctx))
			if w.Code != tc.want {
				t.Fatal("scope wrapper status", w.Code, tc.want)
			}
			if tc.want == 403 && (svc.calls != 0 || plans.calls != 0) {
				t.Fatal("denied request reached operation")
			}
			if strings.Contains(w.Body.String(), "PRIVATE_") {
				t.Fatal("scope wrapper exposed operator-only evidence")
			}
		})
	}
	for _, fixture := range []struct{ method, path, scope string }{{"GET", "/teams/activation", readScope}, {"POST", "/catalog/plans/team-activation", localScope}} {
		r := httptest.NewRequest(fixture.method, fixture.path, nil)
		if _, scope, known := RequiredLocalScope(r); !known || scope != fixture.scope {
			t.Fatal("local policy missing")
		}
		if scope, _, known := RequiredRemoteScope(r); !known || scope != fixture.scope {
			t.Fatal("remote policy missing")
		}
	}
}
