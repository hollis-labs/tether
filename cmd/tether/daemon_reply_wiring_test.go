package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/store"
)

// The reply path is only real once the daemon installs the dispatcher before it
// serves, and hands the server both the HTTP surface and the repair sweep. Nothing
// else reaches daemonRunCmd, so a daemon that forgot either would pass every other
// reply test and quietly answer 404 or never retry.
func TestDaemonRunInstallsReplyRoutingBeforeServing(t *testing.T) {
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
	newDaemonService = func(string) (*app.Service, error) { return svc, nil }
	expected := errors.New("serve failure")
	served := false
	runDaemonServer = func(server *daemon.Server, _ context.Context) error {
		served = true
		if !svc.RoutingReplyWired() {
			t.Error("the daemon serves before the reply dispatcher is installed")
		}
		if server.RoutingReplies == nil {
			t.Error("the server has no reply surface: POST /messages/{id}/reply would answer 404")
		}
		if server.ReplySweeper == nil {
			t.Error("the server has no reply sweep: queued replies for a PTY or in backoff are never retried")
		}
		return expected
	}
	if err := daemonRunCmd.RunE(daemonRunCmd, nil); !errors.Is(err, expected) {
		t.Fatalf("run error: %v", err)
	}
	if !served {
		t.Fatal("the daemon never reached serving")
	}
}
