package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
)

type fakeReloader struct {
	calls  int
	report *config.ReloadReport
	err    error
}

func (f *fakeReloader) ReloadCatalog(context.Context) (*config.ReloadReport, error) {
	f.calls++
	return f.report, f.err
}

func reloadReq(h http.Handler, method string, p *identity.Principal) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/catalog/reload", nil)
	if p != nil {
		req = req.WithContext(identity.WithPrincipal(req.Context(), *p))
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestCatalogReloadRoute(t *testing.T) {
	f := &fakeReloader{report: &config.ReloadReport{Reloaded: true, Added: map[string][]string{"agents": {"fresh"}}}}
	h := NewHandler(Deps{CatalogReload: f})

	rr := reloadReq(h, http.MethodPost, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"reloaded":true`) || !strings.Contains(rr.Body.String(), "fresh") {
		t.Fatalf("reload = %d %s", rr.Code, rr.Body)
	}
	if rr := reloadReq(h, http.MethodGet, nil); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", rr.Code)
	}

	// A session needs catalog.write.
	bare := identity.Principal{Verified: true, ID: "s1", Kind: "session", SessionID: "s1", Scopes: []string{"message.write"}}
	if rr := reloadReq(h, http.MethodPost, &bare); rr.Code != http.StatusForbidden {
		t.Fatalf("session without scope = %d, want 403", rr.Code)
	}
	if f.calls != 1 {
		t.Fatalf("a refused caller reached the reloader: %d calls", f.calls)
	}
	scoped := bare
	scoped.Scopes = append([]string{"catalog.write"}, scoped.Scopes...)
	if rr := reloadReq(h, http.MethodPost, &scoped); rr.Code != http.StatusOK {
		t.Fatalf("session with scope = %d", rr.Code)
	}

	// A catalog that does not validate is a 409 with the reason; nothing is swapped.
	f.report, f.err = nil, errors.New("reload refused, catalog invalid (nothing changed): launch \"x\" names agent \"y\"")
	rr = reloadReq(h, http.MethodPost, nil)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "nothing changed") {
		t.Fatalf("invalid catalog = %d %s", rr.Code, rr.Body)
	}
}

func TestCatalogReloadRouteAbsentWithoutReloader(t *testing.T) {
	if rr := reloadReq(NewHandler(Deps{}), http.MethodPost, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("no reloader = %d, want 404", rr.Code)
	}
}
