package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/store"
)

// The execution routes register only when the wired service implements the
// optional interfaces, so a fake service cannot prove the daemon serves them.
// This builds the server through daemonRunCmd and requests the real handler.
func TestDaemonServesExecutionPolicyAndCapacity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldFactory, oldRun, oldCatalog := newDaemonService, runDaemonServer, catalogPath
	t.Cleanup(func() { newDaemonService, runDaemonServer, catalogPath = oldFactory, oldRun, oldCatalog })
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := &app.Service{Store: db, CatalogRoot: t.TempDir(), Catalog: &config.Catalog{Global: config.Global{Daemon: config.DaemonConfig{ListenAddr: "unix:" + filepath.Join(t.TempDir(), "daemon.sock"), ShutdownTimeout: "1s"}, Identity: config.IdentityConfig{Mode: "off"}}}}
	svc.Channels = channels.New(db, nil)
	catalogPath = filepath.Join(t.TempDir(), "absent-catalog")
	newDaemonService = func(string) (*app.Service, error) { seedDaemonFixtureEventIdentity(t, svc); return svc, nil }
	expected := errors.New("serve failure")
	runDaemonServer = func(server *daemon.Server, _ context.Context) error {
		h := server.Handler()
		for _, path := range []string{"/execution/policy", "/capacity"} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Errorf("GET %s = %d, want 200: %s", path, rec.Code, rec.Body.String())
			}
		}
		return expected
	}
	if err := daemonRunCmd.RunE(daemonRunCmd, nil); !errors.Is(err, expected) {
		t.Fatalf("run error: %v", err)
	}
}
