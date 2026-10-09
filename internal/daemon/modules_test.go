package daemon

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/store"
)

func TestDaemonModuleGateAuthenticatesBeforeHidingDisabledRoutes(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, err := environment.ResolveProfile("worker", map[string]bool{environment.Messaging: false}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Config: Config{Modules: p, IdentityMode: identity.Observe}, Channels: channels.New(db, nil)}
	path := "/channels?as=msg://user/local/observer"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != 404 {
		t.Fatal("disabled messaging reached channel service", w.Code)
	}
	s.Config.Modules = nil
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != 200 {
		t.Fatal("legacy injected configuration changed", w.Code)
	}
	s.Config.Modules = p
	s.Config.IdentityMode = identity.Enforce
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != 401 {
		t.Fatal("module policy bypassed caller authentication", w.Code)
	}
}
