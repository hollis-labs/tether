package main

import (
	"fmt"
	"path/filepath"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/store"
)

func buildEnvironmentDescriptor(cat *config.Catalog, db *store.Store) (*environment.DescriptorHandler, error) {
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
	// Empty capabilities honestly advertise no versioned remote groups yet.
	// Later compositions add groups only when the matching API is installed.
	return environment.NewDescriptor(environment.Descriptor{EnvironmentID: id, Label: cat.Global.Environment.Label, ServerVersion: version, UpdateCapability: "foreground"})
}
