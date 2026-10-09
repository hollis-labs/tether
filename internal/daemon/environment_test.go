package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/environmentstream"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
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

func TestDaemonVersionedStreamsRequireProtocolAndKeepLegacyHealth(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	bus := events.NewBus(events.BusOptions{Persister: db})
	stream := environmentstream.New("fixture-environment", db, bus)
	h := (&Server{Config: Config{IdentityMode: identity.Observe}, EnvironmentStream: stream}).Handler()
	for _, path := range []string{"/environment/snapshot", "/environment/events?after_seq=0", "/sessions/missing/snapshot", "/sessions/missing/stream?after_seq=0"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != 409 {
			t.Fatalf("%s bypassed required protocol: %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/environment/snapshot?protocol=1", nil)
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("stream-only daemon mount failed: %d", w.Code)
	}
	var snapshot struct {
		EnvironmentID string `json:"environment_id"`
	}
	if json.Unmarshal(w.Body.Bytes(), &snapshot) != nil || snapshot.EnvironmentID != "fixture-environment" {
		t.Fatal("stream used different environment identity")
	}
	legacy := httptest.NewRecorder()
	h.ServeHTTP(legacy, httptest.NewRequest(http.MethodGet, "/health", nil))
	if legacy.Code != 200 {
		t.Fatal("bootstrap health requires version")
	}
	unmounted := httptest.NewRecorder()
	h.ServeHTTP(unmounted, httptest.NewRequest(http.MethodGet, "/sessions/missing/unmounted/snapshot", nil))
	if unmounted.Code != 404 {
		t.Fatal("protocol gate widened to unmounted path")
	}
}
