package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every service-token management route the issuer handler serves must be
// registered on the daemon's router. ServeMux matches these paths exactly, and
// a route missing here answers the mux's plain 404 instead of reaching the
// handler: revoke and rotate-key were unreachable on a live hub that way. With
// no caller identity the handler answers 401 with a JSON error, so a 404 or a
// non-JSON body means the request never got there.
func TestServiceTokenManagementRoutesReachTheIssuerHandler(t *testing.T) {
	s, _, _ := remoteTestServer(t)
	s.Issuer = serviceIssuer(t)
	h := s.Handler()
	for _, rt := range []struct{ method, path string }{
		{"POST", "/auth/service-tokens"},
		{"GET", "/auth/service-tokens"},
		{"POST", "/auth/service-tokens/revoke"},
		{"POST", "/auth/service-tokens/rotate-key"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(rt.method, rt.path, strings.NewReader("{}")))
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"unauthorized"`) {
			t.Errorf("%s %s: %d %q, want the handler's 401", rt.method, rt.path, w.Code, w.Body.String())
		}
	}
}

// Registering the subpaths must not open them to the remote listener: the
// operator's own credential is refused there too, on every management route.
func TestServiceTokenManagementRoutesStayLocalOnly(t *testing.T) {
	s, _, operator := remoteTestServer(t)
	s.Issuer = serviceIssuer(t)
	h := s.RemoteHandler("127.0.0.1:7331")
	for _, rt := range []struct{ method, path string }{
		{"POST", "/auth/service-tokens"},
		{"GET", "/auth/service-tokens"},
		{"POST", "/auth/service-tokens/revoke"},
		{"POST", "/auth/service-tokens/rotate-key"},
	} {
		r := httptest.NewRequest(rt.method, "http://localhost:9000"+rt.path, strings.NewReader(`{"jti":"x"}`))
		r.Header.Set("Authorization", "Bearer "+operator)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden && w.Code != http.StatusNotFound && w.Code != http.StatusUnauthorized {
			t.Errorf("remote %s %s with the operator credential: %d %q", rt.method, rt.path, w.Code, w.Body.String())
		}
	}
}
