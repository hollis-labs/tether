package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/environment/report"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/service"
	"github.com/hollis-labs/tether/internal/store"
)

func buildEnvironmentDescriptor(cat *config.Catalog, db *store.Store, capabilities map[string]map[string]any) (*environment.DescriptorHandler, error) {
	if err := cat.Global.Environment.Validate(); err != nil {
		return nil, err
	}
	// The opened selected database is authoritative; do not re-resolve a
	// different fallback state root in a constructor or injected service.
	var stateDB string
	if err := db.DB().QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&stateDB); err != nil {
		return nil, fmt.Errorf("locate environment state: %w", err)
	}
	if stateDB == "" {
		return nil, fmt.Errorf("environment identity requires a persistent state database")
	}
	stateDir := filepath.Dir(stateDB)
	id, err := environment.EnsureID(stateDir)
	if err != nil {
		return nil, err
	}
	if err := environment.BindAuthority(stateDir, id, cat.Global.Environment.Authority); err != nil {
		return nil, err
	}
	// Capabilities describe only the installed and selected startup wiring.
	executable, _ := os.Executable()
	updateCapability := service.LaunchUpdateCapability(os.Getenv(service.RuntimeRootEnv), executable)
	return environment.NewDescriptor(environment.Descriptor{EnvironmentID: id, Label: cat.Global.Environment.Label, ServerVersion: version, UpdateCapability: updateCapability, Capabilities: capabilities})
}

func composedEnvironmentCapabilities(profile *environment.Profile, bus events.Bus, runtimeManager bool) map[string]map[string]any {
	// The report handler is installed by this daemon composition. Advertise
	// its coarse contract without running detectors or exposing their results.
	caps := report.CapabilityGroups()
	if profile.Enabled(environment.StreamAPI) {
		group := map[string]any{"version": 1, "environment_snapshot": true, "session_snapshot": true}
		if _, live := bus.(events.LiveSubscriber); live {
			group["environment_events"], group["session_events"] = true, true
		}
		caps["streams"] = group
	}
	if profile.Enabled(environment.SessionCore) && runtimeManager {
		caps["raw_attach"] = map[string]any{"version": 1, "resume": true}
	}
	return caps
}
