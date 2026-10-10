package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/environment/report"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

func TestServeListenerOverridesAndCatalogIsolation(t *testing.T) {
	cat := &config.Catalog{Global: config.Global{Identity: config.IdentityConfig{Mode: "off"}, Daemon: config.DaemonConfig{ListenAddr: "unix:/unused", ShutdownTimeout: "1s"}}}
	cmd := &cobra.Command{Use: "serve"}
	addRemoteServeFlags(cmd)
	cfg, err := daemonConfigFromCommand(cat, cmd)
	if err != nil || !cfg.RemoteListener.Enabled || cfg.RemoteListener.ListenAddr != daemon.DefaultRemoteListenAddr || cfg.IdentityMode != identity.Off {
		t.Fatal(cfg, err)
	}
	for name, value := range map[string]string{"remote-listen": "tcp:[::1]:7332", "allowed-host": "localhost:9000", "allowed-origin": "https://worker.example"} {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err = daemonConfigFromCommand(cat, cmd)
	if err != nil || cfg.RemoteListener.ListenAddr != "tcp:[::1]:7332" || cfg.RemoteListener.AllowedHosts[0] != "localhost:9000" || cfg.RemoteListener.AllowedOrigins[0] != "https://worker.example" {
		t.Fatal(cfg, err)
	}
	if cat.Global.Daemon.RemoteListener.Enabled || cat.Global.Daemon.RemoteListener.ListenAddr != "" {
		t.Fatal("flags mutated catalog")
	}
	if err := cmd.Flags().Set("remote-listen", "tcp:0.0.0.0:7331"); err != nil {
		t.Fatal(err)
	}
	if _, err := daemonConfigFromCommand(cat, cmd); err == nil {
		t.Fatal("nonloopback override accepted")
	}
	if err := cmd.Flags().Set("remote-listen", daemon.DefaultRemoteListenAddr); err != nil {
		t.Fatal(err)
	}
	cat.Global.Modules = map[string]bool{environment.RemoteListener: false}
	if _, err := daemonConfigFromCommand(cat, cmd); err == nil {
		t.Fatal("serve bypassed module policy")
	}
	if found, _, err := rootCmd.Find([]string{"serve"}); err != nil || found != serveCmd {
		t.Fatal(err)
	}
}

func TestServeComposesRemoteIdentityWithLocalOff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	savedFactory, savedRun, savedClose, savedCatalog := newDaemonService, runDaemonServer, closeDaemonService, catalogPath
	savedReport := newDaemonEnvironmentReport
	t.Cleanup(func() {
		newDaemonService, runDaemonServer, closeDaemonService, catalogPath = savedFactory, savedRun, savedClose, savedCatalog
		newDaemonEnvironmentReport = savedReport
	})
	newDaemonEnvironmentReport = func(context.Context, *app.Service, daemon.Config, app.ProtectionHealth) (*report.API, func(), error) {
		return &report.API{}, func() {}, nil
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cat := &config.Catalog{Global: config.Global{Role: "worker", Identity: config.IdentityConfig{Mode: "off"}, Daemon: config.DaemonConfig{ListenAddr: "unix:" + filepath.Join(t.TempDir(), "unused.sock"), ShutdownTimeout: "1s"}}}
	svc := &app.Service{Store: db, Catalog: cat, CatalogRoot: t.TempDir()}
	catalogPath = filepath.Join(t.TempDir(), "unused-catalog")
	created, closed, ran := 0, 0, 0
	newDaemonService = func(string) (*app.Service, error) { created++; return svc, nil }
	closeDaemonService = func(*app.Service) error { closed++; return nil }
	reached := errors.New("test foreground daemon composed")
	runDaemonServer = func(s *daemon.Server, _ context.Context) error {
		ran++
		if s.Identity == nil || s.Config.IdentityMode != identity.Off || !s.Config.RemoteListener.Enabled {
			t.Fatal("remote store missing or local policy changed")
		}
		exists, err := s.Identity.HasPrincipal(context.Background(), identity.OperatorID)
		if err != nil || exists {
			t.Fatal("local-off remote startup bootstrapped operator", err)
		}
		return reached
	}
	if err := serveCmd.RunE(serveCmd, nil); !errors.Is(err, reached) {
		t.Fatal(err)
	}
	if created != 1 || ran != 1 || closed != 1 {
		t.Fatal("serve did not share one daemon lifecycle", created, ran, closed)
	}
}

func TestServeRemoteYAMLConfiguration(t *testing.T) {
	var global config.Global
	err := yaml.Unmarshal([]byte(`identity:
  mode: observe
daemon:
  listen_addr: unix:/unused
  shutdown_timeout: 1s
  remote_listener:
    enabled: true
    listen_addr: tcp:127.0.0.1:7331
    allowed_hosts: [localhost:9000]
    allowed_origins: [http://localhost:9000]
`), &global)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := daemonConfigFromCatalog(&config.Catalog{Global: global})
	if err != nil || !cfg.RemoteListener.Enabled || cfg.IdentityMode != identity.Observe || cfg.RemoteListener.AllowedHosts[0] != "localhost:9000" || cfg.RemoteListener.AllowedOrigins[0] != "http://localhost:9000" {
		t.Fatal(cfg, err)
	}
}
