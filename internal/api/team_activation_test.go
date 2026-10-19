package api

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
