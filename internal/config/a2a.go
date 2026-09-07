package config

// a2a.go — T10 (messaging vNext, CW-20260906-0041): opt-in catalog config
// for the A2A adapter (internal/a2aadapter). Files live at
// <catalogDir>/a2a/*.yaml, mirroring mcp_server.go's shape exactly (the
// established pattern in this package for an "opt-in list of external
// integrations" catalog): Enabled defaults to true when absent, and
// credential-bearing fields accept a keychain://.../helper://... secret
// reference resolved only by the spawn-time loader, never the catalog
// display loader.
//
// An empty or missing a2a/ directory means the A2A surface is absent
// entirely -- "the feature stays optional for local messaging" (T10
// acceptance #3).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// A2ABindingEntry describes one Tether agent opted into A2A reachability.
// Deliberately plain (no internal/a2aadapter dependency) -- the
// composition root (cmd/mux/daemon.go) converts this into
// a2aadapter.AgentBinding, keeping this package's dependency direction
// unchanged (config does not import adapter packages).
type A2ABindingEntry struct {
	ID          string `yaml:"id"`
	TargetURN   string `yaml:"target_urn"`
	DisplayName string `yaml:"display_name"`
	Description string `yaml:"description"`
	BaseURL     string `yaml:"base_url"`
	// BearerToken accepts a literal, a ${VAR} reference, or a secret
	// reference (keychain://... / helper://...), resolved the same way
	// MCPServerEntry.Token is (see resolveSecretRef).
	BearerToken             string `yaml:"bearer_token"`
	TaskMode                bool   `yaml:"task_mode"`
	TaskAwaitTimeoutSeconds int    `yaml:"task_await_timeout_seconds"`
	Enabled                 *bool  `yaml:"enabled"` // nil → defaults to true
}

// IsEnabled returns true when the entry should be loaded. A missing
// enabled field (nil pointer) is treated as true.
func (e *A2ABindingEntry) IsEnabled() bool {
	return e.Enabled == nil || *e.Enabled
}

// TaskAwaitTimeout converts TaskAwaitTimeoutSeconds to a time.Duration;
// zero or negative means "use the adapter's own default."
func (e *A2ABindingEntry) TaskAwaitTimeout() time.Duration {
	if e.TaskAwaitTimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(e.TaskAwaitTimeoutSeconds) * time.Second
}

// LoadA2ABindingCatalog reads all A2A binding catalog entries, including
// disabled ones. GUI/config surfaces use this so disabled bindings remain
// visible and can be re-enabled. A missing a2a/ directory is silently
// treated as empty (matches LoadMCPServerCatalog's convention).
func LoadA2ABindingCatalog(catalogDir string) ([]A2ABindingEntry, error) {
	dir := filepath.Join(catalogDir, "a2a")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read a2a dir: %w", err)
	}

	var out []A2ABindingEntry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}

		path := filepath.Join(dir, name)
		b, err := os.ReadFile(path) //nolint:gosec // G304: catalog-sourced path
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}

		var entry A2ABindingEntry
		if err := yaml.Unmarshal(b, &entry); err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}

		entry.BaseURL = expandEnvRefs(entry.BaseURL)
		entry.BearerToken = expandEnvRefs(entry.BearerToken)

		out = append(out, entry)
	}
	return out, nil
}

// LoadA2ABindings reads the catalog, resolves BearerToken secret
// references, and returns the enabled entries -- the spawn-time loader
// every daemon-construction call site should use (matches
// LoadMCPServers's split between "display" and "spawn" loaders, so
// resolved secret material never reaches a catalog display/edit surface,
// which reads LoadA2ABindingCatalog's unresolved entries instead).
func LoadA2ABindings(catalogDir string) ([]A2ABindingEntry, error) {
	entries, err := LoadA2ABindingCatalog(catalogDir)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	out := entries[:0]
	for _, entry := range entries {
		if !entry.IsEnabled() {
			continue
		}
		var err error
		if entry.BearerToken, err = resolveSecretRef(ctx, "bearer_token", entry.BearerToken); err != nil {
			return nil, fmt.Errorf("a2a binding %q: %w", entry.ID, err)
		}
		out = append(out, entry)
	}
	return out, nil
}
