package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/environment/report"
	"github.com/hollis-labs/tether/internal/store"
)

func TestDaemonEnvironmentReportSelectedInputsAndJoinedSampler(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stateRoot, workRoot := t.TempDir(), t.TempDir()
	db, err := store.Open(filepath.Join(stateRoot, "selected.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	profile, err := environment.ResolveProfile("worker", nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	svc := &app.Service{Store: db, Catalog: &config.Catalog{Global: config.Global{Catalog: config.CatalogRoots{Defaults: config.Defaults{
		StateDB: filepath.Join(t.TempDir(), "unopened.db"), WorkspaceRoot: workRoot, LaunchHost: "shim",
	}}}}}
	health := app.ProtectionHealth{ProtectionStatus: app.ProtectionStatus{Enabled: true}, BwrapUsable: true, Codex: app.CodexProtectionState{State: "not protected"}}
	detectors := &report.Detectors{}
	entered, release := make(chan [2]string, 1), make(chan struct{})
	api, stop, err := buildDaemonEnvironmentReport(context.Background(), svc, daemon.Config{Modules: profile}, health, environmentReportDeps{
		detectors: detectors,
		measure: func(state, work string) report.ResourceState {
			entered <- [2]string{state, work}
			<-release
			return report.ResourceState{CPUCount: 2}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Release even on a failed assertion, then join the owned sampler.
	t.Cleanup(stop)
	t.Cleanup(func() { close(release) })
	if api.StateRoot != stateRoot || api.WorkRoot != workRoot || api.Profile != profile || api.Detectors != detectors || !api.LaunchHostShim {
		t.Fatal("report did not use selected state/workspace/profile inputs")
	}
	if !api.SandboxData.Enabled || !api.SandboxData.BwrapUsable || api.SandboxData.Codex != "not protected" {
		t.Fatal("report replaced actual protection data")
	}
	select {
	case roots := <-entered:
		if roots != [2]string{stateRoot, workRoot} {
			t.Fatal("sampler measured a different volume", roots)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sampler did not start")
	}
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop returned while owned measurement was blocked")
	case <-time.After(20 * time.Millisecond):
	}
	// The cleanup owns closing release; run it now without closing twice.
	release <- struct{}{}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not join sampler after measurement completed")
	}
	stop() // repeat shutdown is safe
	if got := api.Sampler.Current(); got.CPUCount != 2 {
		t.Fatal("sampler lost final measurement", got)
	}
}

func TestDaemonEnvironmentReportPanicDegradesAndJoins(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	measured := make(chan struct{})
	api, stop, err := buildDaemonEnvironmentReport(context.Background(), &app.Service{Store: db, Catalog: &config.Catalog{}}, daemon.Config{}, app.ProtectionHealth{}, environmentReportDeps{
		measure: func(string, string) report.ResourceState {
			close(measured)
			panic("synthetic sampler failure")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	select {
	case <-measured:
	case <-time.After(5 * time.Second):
		t.Fatal("sampler did not measure")
	}
	stop()
	if got := api.Sampler.Current(); got.CPULoad != "unknown" {
		t.Fatal("sampler failure did not degrade report", got)
	}
	if err := db.DB().PingContext(context.Background()); err != nil {
		t.Fatal("sampler failure closed daemon state", err)
	}
}

func TestDaemonEnvironmentReportCloseBeforeServiceOnRunFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(app.ProtectEnv, "0")
	oldFactory, oldClose, oldRun, oldCatalog, oldReport := newDaemonService, closeDaemonService, runDaemonServer, catalogPath, newDaemonEnvironmentReport
	t.Cleanup(func() {
		newDaemonService, closeDaemonService, runDaemonServer, catalogPath, newDaemonEnvironmentReport = oldFactory, oldClose, oldRun, oldCatalog, oldReport
	})
	for _, useCloseHook := range []bool{false, true} {
		t.Run(map[bool]string{false: "startup failure", true: "ordered close hook"}[useCloseHook], func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			svc := &app.Service{Store: db, CatalogRoot: t.TempDir(), Catalog: &config.Catalog{Global: config.Global{
				Role: "worker", Daemon: config.DaemonConfig{ListenAddr: "unix:" + filepath.Join(t.TempDir(), "unused.sock"), ShutdownTimeout: "1s"}, Identity: config.IdentityConfig{Mode: "off"},
				Catalog: config.CatalogRoots{Defaults: config.Defaults{WorkspaceRoot: t.TempDir()}},
			}}}
			catalogPath = filepath.Join(t.TempDir(), "absent-catalog")
			newDaemonService = func(string) (*app.Service, error) { return svc, nil }
			var stopped atomic.Bool
			newDaemonEnvironmentReport = func(ctx context.Context, got *app.Service, cfg daemon.Config, health app.ProtectionHealth) (*report.API, func(), error) {
				api, stop, err := buildDaemonEnvironmentReport(ctx, got, cfg, health, environmentReportDeps{
					measure: func(string, string) report.ResourceState { return report.ResourceState{CPUCount: 1} },
				})
				return api, func() { stop(); stopped.Store(true) }, err
			}
			closed := 0
			closeDaemonService = func(got *app.Service) error {
				if got != svc || !stopped.Load() {
					t.Fatal("service closed before sampler cancellation/join")
				}
				closed++
				return got.Close()
			}
			expected := errors.New("synthetic serve failure")
			runDaemonServer = func(server *daemon.Server, _ context.Context) error {
				if server.EnvironmentReport == nil {
					t.Fatal("command did not compose report into server")
				}
				if useCloseHook {
					if err := server.Close(); err != nil {
						t.Fatal(err)
					}
					if err := server.Close(); err != nil {
						t.Fatal(err)
					}
				}
				return expected
			}
			if err := daemonRunCmd.RunE(daemonRunCmd, nil); !errors.Is(err, expected) {
				t.Fatalf("run error %v", err)
			}
			if closed != 1 {
				t.Fatalf("service closed %d times", closed)
			}
		})
	}
}
