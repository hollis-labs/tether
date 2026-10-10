package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/environment/report"
)

type environmentReportDeps struct {
	detectors *report.Detectors
	measure   func(stateRoot, workRoot string) report.ResourceState
}

// The seam allows command lifecycle tests to use only synthetic detectors.
var newDaemonEnvironmentReport = func(ctx context.Context, svc *app.Service, cfg daemon.Config, health app.ProtectionHealth) (*report.API, func(), error) {
	return buildDaemonEnvironmentReport(ctx, svc, cfg, health, environmentReportDeps{})
}

func buildDaemonEnvironmentReport(ctx context.Context, svc *app.Service, cfg daemon.Config, health app.ProtectionHealth, deps environmentReportDeps) (*report.API, func(), error) {
	if svc == nil || svc.Store == nil || svc.Catalog == nil {
		return nil, nil, fmt.Errorf("environment report requires selected state and catalog")
	}
	// Match environment identity: the opened DB, rather than a re-resolved
	// catalog fallback, decides which state volume the report describes.
	var stateDB string
	if err := svc.Store.DB().QueryRowContext(ctx, "SELECT file FROM pragma_database_list WHERE name='main'").Scan(&stateDB); err != nil {
		return nil, nil, fmt.Errorf("locate environment report state: %w", err)
	}
	if stateDB == "" {
		return nil, nil, fmt.Errorf("environment report requires a persistent state database")
	}
	stateRoot := filepath.Dir(stateDB)
	defaults := svc.Catalog.Global.Catalog.Defaults
	workRoot := config.ResolveWorkspaceRoot(defaults, svc.Catalog.Paths)
	if deps.detectors == nil {
		deps.detectors = report.DefaultDetectors()
	}
	if deps.measure == nil {
		deps.measure = deps.detectors.MeasureResources
	}
	sampler := report.NewSampler(10 * time.Second)
	samplingCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sampler.Start(samplingCtx, func() report.ResourceState {
			return deps.measure(stateRoot, workRoot)
		})
	}()
	stop := func() {
		cancel()
		<-done
	}
	return &report.API{
		Detectors: deps.detectors,
		Sampler:   sampler,
		SandboxData: report.SandboxProtectData{
			Enabled:         health.Enabled,
			BwrapUsable:     health.BwrapUsable,
			BwrapChecked:    health.BwrapChecked,
			PlanUnavailable: health.PlanError != "",
			Codex:           health.Codex.State,
		},
		LaunchHostShim: defaults.LaunchHost == "shim",
		StateRoot:      stateRoot,
		WorkRoot:       workRoot,
		Profile:        cfg.Modules,
	}, stop, nil
}
