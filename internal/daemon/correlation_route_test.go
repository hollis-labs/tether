package daemon

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/identity"
)

type correlationRouteStub struct{ calls int }

func (s *correlationRouteStub) RegisterExternalCorrelation(_ context.Context, _ api.CorrelationRegisterRequest) (api.CorrelationRegisterResponse, error) {
	s.calls++
	return api.CorrelationRegisterResponse{Queued: true}, nil
}

func TestDaemonHandlerCorrelationRoute(t *testing.T) {
	stub := &correlationRouteStub{}
	s := &Server{Correlations: stub}
	h := s.Handler()
	call := func(ctx context.Context) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/correlation/register", strings.NewReader(`{"urn":"msg://agent/local/kit","session":"external"}`)).WithContext(ctx)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := call(context.Background()); w.Code != 403 {
		t.Fatal("anonymous", w.Code, w.Body.String())
	}
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator"})
	if w := call(ctx); w.Code != 202 {
		t.Fatal("operator", w.Code, w.Body.String())
	}
	if stub.calls != 1 {
		t.Fatal("registrar calls", stub.calls)
	}
}
