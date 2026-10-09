package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/identity"
)

func TestPublicEnvironmentDescriptorOnEnforcedDaemon(t *testing.T) {
	id, err := environment.EnsureID(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := environment.NewDescriptor(environment.Descriptor{EnvironmentID: id, Label: "isolated", ServerVersion: "test-version"})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Config: Config{IdentityMode: identity.Enforce}, Environment: descriptor}
	h := s.Handler()
	for _, path := range []string{environment.DescriptorPath, "/health"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set(environment.ProtocolHeader, "999")
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("public bootstrap %s status %d", path, w.Code)
		}
		var response struct {
			EnvironmentID string `json:"environmentId"`
			ServerVersion string `json:"serverVersion"`
			Protocol      int    `json:"protocol"`
		}
		if json.Unmarshal(w.Body.Bytes(), &response) != nil || response.EnvironmentID != id || response.ServerVersion != "test-version" || response.Protocol != environment.Protocol {
			t.Fatalf("%s identity differs from descriptor", path)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	if w.Code != 401 {
		t.Fatal("descriptor bypass widened identity policy")
	}
	post := httptest.NewRecorder()
	h.ServeHTTP(post, httptest.NewRequest(http.MethodPost, environment.DescriptorPath, nil))
	if post.Code != 405 {
		t.Fatal("descriptor became mutable")
	}
}

func TestLocalProtocolMismatchIsTypedAndLegacyMissingAllowed(t *testing.T) {
	h := (&Server{Config: Config{IdentityMode: identity.Observe}}).Handler()
	legacy := httptest.NewRecorder()
	h.ServeHTTP(legacy, httptest.NewRequest(http.MethodGet, "/not-mounted", nil))
	if legacy.Code != 404 {
		t.Fatal("local legacy client changed")
	}
	r := httptest.NewRequest(http.MethodGet, "/not-mounted", nil)
	r.Header.Set(environment.ProtocolHeader, "2")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 409 {
		t.Fatal("mismatched client reached API")
	}
}
