package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load reads the global catalog + subdirectories and returns a populated Catalog.
// catalogRoot is a directory containing global.yaml and the subfolders defined by it.
func Load(catalogRoot string) (*Catalog, error) {
	catalogRoot = Expand(catalogRoot)
	cat := &Catalog{
		Projects:  map[string]Project{},
		Agents:    map[string]Agent{},
		Providers: map[string]Provider{},
		Launches:  map[string]Launch{},
	}
	if err := loadYAML(filepath.Join(catalogRoot, "global.yaml"), &cat.Global); err != nil {
		return nil, fmt.Errorf("load global: %w", err)
	}
	applyDaemonDefaults(&cat.Global.Daemon)

	roots := cat.Global.Catalog.Roots
	if err := loadDir(resolveRoot(catalogRoot, roots.Projects, "projects"), func(path string) error {
		var p Project
		if err := loadYAML(path, &p); err != nil {
			return err
		}
		cat.Projects[p.ID] = p
		return nil
	}); err != nil {
		return nil, err
	}
	if err := loadDir(resolveRoot(catalogRoot, roots.Agents, "agents"), func(path string) error {
		var a Agent
		if err := loadYAML(path, &a); err != nil {
			return err
		}
		cat.Agents[a.ID] = a
		return nil
	}); err != nil {
		return nil, err
	}
	if err := loadDir(resolveRoot(catalogRoot, roots.Providers, "providers"), func(path string) error {
		var pr Provider
		if err := loadYAML(path, &pr); err != nil {
			return err
		}
		cat.Providers[pr.ID] = pr
		return nil
	}); err != nil {
		return nil, err
	}
	if err := loadDir(resolveRoot(catalogRoot, roots.Launches, "launches"), func(path string) error {
		var l Launch
		if err := loadYAML(path, &l); err != nil {
			return err
		}
		cat.Launches[l.ID] = l
		return nil
	}); err != nil {
		return nil, err
	}
	return cat, nil
}

// applyDaemonDefaults fills in listen_addr / pid_file / shutdown_timeout when
// the catalog's global.yaml omits them. Paths are left un-expanded; callers
// that need filesystem paths should run them through config.Expand.
func applyDaemonDefaults(d *DaemonConfig) {
	if d.ListenAddr == "" {
		d.ListenAddr = "unix:~/.agent-mux/run/muxd.sock"
	}
	if d.PIDFile == "" {
		d.PIDFile = "~/.agent-mux/run/muxd.pid"
	}
	if d.ShutdownTimeout == "" {
		d.ShutdownTimeout = "10s"
	}
}

func resolveRoot(catalogRoot, fromGlobal, fallback string) string {
	if fromGlobal != "" {
		if filepath.IsAbs(fromGlobal) || strings.HasPrefix(fromGlobal, "~") {
			return Expand(fromGlobal)
		}
		return filepath.Join(catalogRoot, fromGlobal)
	}
	return filepath.Join(catalogRoot, fallback)
}

func loadYAML(path string, out any) error {
	// Catalog paths originate from the user's own catalog directory, not
	// from an HTTP/network boundary.
	b, err := os.ReadFile(path) //nolint:gosec // G304: catalog-sourced path

	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(b, out); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func loadDir(dir string, fn func(path string) error) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if filepath.Ext(name) != ".yaml" && filepath.Ext(name) != ".yml" {
			continue
		}
		if err := fn(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("load %s: %w", name, err)
		}
	}
	return nil
}
