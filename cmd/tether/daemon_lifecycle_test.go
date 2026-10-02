package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"errors"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/store"
)

func TestDaemonRunStartupFailuresCloseServiceOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	savedFactory, savedClose, savedCatalog := newDaemonService, closeDaemonService, catalogPath
	t.Cleanup(func() { newDaemonService, closeDaemonService, catalogPath = savedFactory, savedClose, savedCatalog })
	for _, tc := range []struct {
		name, want string
		change     func(*config.Catalog)
	}{
		{"ownership", "mcp_upstream_ownership", func(c *config.Catalog) { c.Global.Daemon.MCPUpstreams = "invalid" }},
		{"config", "shutdown_timeout", func(c *config.Catalog) { c.Global.Daemon.ShutdownTimeout = "invalid" }},
		{"bind policy", "identity", func(c *config.Catalog) { c.Global.Identity.Mode = "invalid" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			cat := &config.Catalog{Global: config.Global{Daemon: config.DaemonConfig{ListenAddr: "unix:" + filepath.Join(t.TempDir(), "daemon.sock"), ShutdownTimeout: "1s"}, Identity: config.IdentityConfig{Mode: "off"}}}
			tc.change(cat)
			svc := &app.Service{Catalog: cat, Store: db}
			catalogPath = filepath.Join(t.TempDir(), "absent-catalog")
			newDaemonService = func(string) (*app.Service, error) { return svc, nil }
			closed := 0
			closeDaemonService = func(got *app.Service) error {
				if got != svc {
					t.Fatal("closed wrong service")
				}
				if err := db.DB().PingContext(context.Background()); err != nil {
					t.Fatal("storage bypassed ordered close", err)
				}
				closed++
				return got.Close()
			}
			if err := daemonRunCmd.RunE(daemonRunCmd, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("startup error: %v", err)
			}
			if closed != 1 {
				t.Fatalf("service close invoked %d times", closed)
			}
			if err := db.DB().PingContext(context.Background()); err == nil {
				t.Fatal("startup left storage open")
			}
		})
	}
}

func TestDaemonRunSharesChannelsAndOrdersCloseHook(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldFactory, oldClose, oldRun, oldCatalog := newDaemonService, closeDaemonService, runDaemonServer, catalogPath
	t.Cleanup(func() {
		newDaemonService, closeDaemonService, runDaemonServer, catalogPath = oldFactory, oldClose, oldRun, oldCatalog
	})
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := &app.Service{Store: db, CatalogRoot: t.TempDir(), Catalog: &config.Catalog{Global: config.Global{Daemon: config.DaemonConfig{ListenAddr: "unix:" + filepath.Join(t.TempDir(), "daemon.sock"), ShutdownTimeout: "1s"}, Identity: config.IdentityConfig{Mode: "off"}}}}
	svc.Channels = channels.New(db, nil)
	catalogPath = filepath.Join(t.TempDir(), "absent-catalog")
	newDaemonService = func(string) (*app.Service, error) { return svc, nil }
	closed := 0
	closeDaemonService = func(got *app.Service) error {
		if got != svc {
			t.Fatal("wrong service")
		}
		if err := db.DB().PingContext(context.Background()); err != nil {
			t.Fatal("storage closed before service", err)
		}
		closed++
		return got.Close()
	}
	expected := errors.New("serve failure")
	runDaemonServer = func(server *daemon.Server, _ context.Context) error {
		if server.Channels != svc.Channels {
			t.Fatal("server and router do not share channel service")
		}
		if server.Close == nil {
			t.Fatal("server has no ordered close hook")
		}
		if err := server.Close(); err != nil {
			t.Fatal(err)
		}
		if err := server.Close(); err != nil {
			t.Fatal(err)
		}
		return expected
	}
	if err := daemonRunCmd.RunE(daemonRunCmd, nil); !errors.Is(err, expected) {
		t.Fatalf("run error: %v", err)
	}
	if closed != 1 {
		t.Fatalf("ordered close called %d times", closed)
	}
}
