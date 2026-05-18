package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hollis-labs/go-apppaths/paths"
	"github.com/hollis-labs/go-sandbox/sandbox"
)

// Load reads the global catalog + subdirectories and returns a populated Catalog.
// catalogRoot is a directory containing global.yaml and the subfolders defined by it.
func Load(catalogRoot string) (*Catalog, error) {
	catalogRoot = Expand(catalogRoot)
	cat := &Catalog{
		Projects:        map[string]Project{},
		Agents:          map[string]Agent{},
		Providers:       map[string]Provider{},
		Launches:        map[string]Launch{},
		SandboxProfiles: map[string]sandbox.Profile{},
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

	// Sandbox profiles are optional — missing dir is not an error.
	profiles, err := sandbox.LoadProfiles(filepath.Join(catalogRoot, "sandbox-profiles"))
	if err != nil {
		return nil, fmt.Errorf("load sandbox-profiles: %w", err)
	}
	cat.SandboxProfiles = profiles

	// Resolve the go-apppaths Layout once per Load. It backs the FALLBACK
	// storage paths (state_db / workspace_root / temp_root) used only when
	// global.yaml omits the corresponding catalog default. WithoutMaterialize
	// keeps a plain Load (including read-only commands and `mux path`) from
	// creating ~/.local/share/tether/...; the actual consumers (store.Open,
	// workspace.Materialize*) create the directories they need when the
	// fallback path is reached.
	layout, err := ResolveLayout(paths.WithoutMaterialize())
	if err != nil {
		return nil, fmt.Errorf("resolve layout: %w", err)
	}
	cat.Paths = layout

	return cat, nil
}

// LoadLayered loads the catalog like Load, then overlays agents discovered in
// the user and per-project discovery layers. Use it anywhere the catalog's
// launches are resolved or validated.
//
// Plain Load only reads agents from the system-catalog root
// (<catalogRoot>/agents/). But `mux agents create` and project-local config
// write agents into the user layer (~/.tether/agents/) or a project layer
// (<repo_root>/.tether/agents/) instead. A launch referencing such an agent
// would otherwise fail validation with "references unknown agent" even though
// the agent exists — the agent is simply invisible to the single-root loader.
// LoadLayered closes that gap so layered agents are first-class for launches.
//
// Precedence (later wins): system catalog < user layer < project layer. When
// two projects define the same agent ID, projects are merged in sorted ID
// order so the result is deterministic.
func LoadLayered(catalogRoot string) (*Catalog, error) {
	catalogRoot = Expand(catalogRoot)
	cat, err := Load(catalogRoot)
	if err != nil {
		return nil, err
	}
	// The system layer is already populated by Load above; Discover here
	// only walks the user + project layers so it is not read twice.
	var layers []LayerSpec
	if home, err := os.UserHomeDir(); err == nil {
		layers = append(layers, LayerSpec{Layer: LayerUser, Root: filepath.Join(home, ".tether")})
	}
	projectIDs := make([]string, 0, len(cat.Projects))
	for id := range cat.Projects {
		projectIDs = append(projectIDs, id)
	}
	sort.Strings(projectIDs)
	for _, id := range projectIDs {
		repoRoot := cat.Projects[id].RepoRoot
		if repoRoot == "" {
			continue
		}
		layers = append(layers, LayerSpec{
			Layer: LayerProject,
			Root:  filepath.Join(Expand(repoRoot), ".tether"),
		})
	}
	layered, err := Discover(layers)
	if err != nil {
		return nil, fmt.Errorf("discover layered agents: %w", err)
	}
	for id, la := range layered.Agents {
		cat.Agents[id] = la.Agent
	}
	return cat, nil
}

// applyDaemonDefaults fills in listen_addr / pid_file / shutdown_timeout when
// the catalog's global.yaml omits them. Paths are left un-expanded; callers
// that need filesystem paths should run them through config.Expand.
func applyDaemonDefaults(d *DaemonConfig) {
	if d.ListenAddr == "" {
		d.ListenAddr = "unix:~/.tether/run/muxd.sock"
	}
	if d.PIDFile == "" {
		d.PIDFile = "~/.tether/run/muxd.pid"
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
