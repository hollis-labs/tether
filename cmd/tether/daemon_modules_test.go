package main

import (
	"context"
	"errors"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/spf13/cobra"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/environment"
)

func TestWorkerCompositionSkipsHubModulesAndReportsUnavailable(t *testing.T) {
	cat := &config.Catalog{Global: config.Global{Role: "worker", Teams: config.TeamsConfig{Enabled: true}, AI: config.AIConfig{Providers: []config.AIProviderConfig{{ID: "configured"}}}, Daemon: config.DaemonConfig{ListenAddr: "unix:/unused", ShutdownTimeout: "1s"}}}
	cfg, err := daemonConfigFromCatalog(cat)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TeamsEnabled || cfg.Modules.Enabled(environment.LLMGateway) {
		t.Fatal("worker inherited hub opt-ins")
	}
	// No service/store/catalog root is needed when team composition is disabled.
	teams, closeTeams, err := buildDaemonTeams(nil, cfg)
	if err != nil || teams != nil {
		t.Fatal(teams, err)
	}
	closeTeams()
	if ai := buildAIServiceFromConfig(context.Background(), cat, aiServiceDeps{}); ai != nil {
		t.Fatal("worker composed AI")
	}
	for _, c := range checkRoleModules(cat) {
		if c.Name == "module:llm_gateway" && c.Message != "off" {
			t.Fatal(c)
		}
	}
	cat.Global.Role = "hub"
	checks := checkRoleModules(cat)
	found := false
	for _, c := range checks {
		if c.Name == "module:environment_directory" {
			found = true
			if !strings.Contains(c.Message, "not installed") {
				t.Fatal(c)
			}
		}
	}
	if !found {
		t.Fatal("doctor omitted future module status")
	}
}

func TestLegacyTeamsAndRemoteListenerConfiguration(t *testing.T) {
	cat := &config.Catalog{Global: config.Global{Daemon: config.DaemonConfig{ListenAddr: "tcp:127.0.0.1:0", ShutdownTimeout: "1s"}}}
	cfg, err := daemonConfigFromCatalog(cat)
	if err != nil || cfg.TeamsEnabled {
		t.Fatal(cfg, err)
	}
	cat.Global.Teams.Enabled = true
	cfg, err = daemonConfigFromCatalog(cat)
	if err != nil || !cfg.TeamsEnabled {
		t.Fatal(cfg, err)
	}
	cat.Global.Modules = map[string]bool{environment.RemoteListener: false}
	if _, err = daemonConfigFromCatalog(cat); err == nil {
		t.Fatal("disabled TCP listener accepted")
	}
}

func TestWorkerDaemonCompositionStartsWithoutHubModules(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	savedFactory, savedRun, savedCatalog := newDaemonService, runDaemonServer, catalogPath
	t.Cleanup(func() { newDaemonService, runDaemonServer, catalogPath = savedFactory, savedRun, savedCatalog })
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cat := &config.Catalog{Global: config.Global{Role: "worker", Teams: config.TeamsConfig{Enabled: true}, AI: config.AIConfig{Providers: []config.AIProviderConfig{{ID: "configured"}}}, Daemon: config.DaemonConfig{ListenAddr: "unix:" + filepath.Join(t.TempDir(), "unused.sock"), ShutdownTimeout: "1s"}, Identity: config.IdentityConfig{Mode: "off"}}}
	svc := &app.Service{Store: db, Catalog: cat, CatalogRoot: t.TempDir()}
	catalogPath = filepath.Join(t.TempDir(), "unused-catalog")
	newDaemonService = func(string) (*app.Service, error) { return svc, nil }
	reached := errors.New("composed worker")
	runDaemonServer = func(s *daemon.Server, _ context.Context) error {
		if api.HasTeamOps(s.Teams) || s.AI != nil || s.Config.TeamsEnabled || s.Config.Modules.Role != "worker" || s.Environment == nil {
			t.Fatal("worker composition inherited hub modules")
		}
		return reached
	}
	if err := daemonRunCmd.RunE(daemonRunCmd, nil); !errors.Is(err, reached) {
		t.Fatal("worker failed before serving", err)
	}
}

func TestWorkerTeamCLIUsesSameProfile(t *testing.T) {
	dir := writeCatalog(t, t.TempDir(), "version: 1\nrole: worker\nteams:\n  enabled: true\n")
	root := buildRootCommand(&cobra.Command{Use: "tether"}, []string{"--catalog", dir}, "unused")
	for _, cmd := range root.Commands() {
		if cmd.Name() == "team" {
			t.Fatal("worker exposed disabled teams")
		}
	}
}
