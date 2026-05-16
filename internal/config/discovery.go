package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// Layer identifies which discovery layer a catalog entry originated from.
// Lower-precedence layers come first; later layers win on ID collision.
type Layer int

const (
	// LayerSystem is the bundled Mux catalog (the existing single-root catalog).
	LayerSystem Layer = iota
	// LayerUser is ~/.tether/ — personal customization.
	LayerUser
	// LayerProject is ./.tether/ — repo-local, highest precedence.
	LayerProject
)

func (l Layer) String() string {
	switch l {
	case LayerSystem:
		return "system"
	case LayerUser:
		return "user"
	case LayerProject:
		return "project"
	default:
		return fmt.Sprintf("layer(%d)", int(l))
	}
}

// LayerSpec is a single discovery layer's root directory. Roots that do not
// exist on disk are skipped silently — discovery is best-effort by design.
type LayerSpec struct {
	Layer Layer
	Root  string
}

// DefaultLayers returns the canonical [system, user, project] discovery layer
// stack. `systemRoot` is the bundled catalog directory (commonly the value
// passed to `Load`). `workingDir` anchors the project layer (typically the
// daemon's CWD or the launch's project root).
func DefaultLayers(systemRoot, workingDir string) []LayerSpec {
	layers := []LayerSpec{
		{Layer: LayerSystem, Root: Expand(systemRoot)},
	}
	if home, err := os.UserHomeDir(); err == nil {
		layers = append(layers, LayerSpec{Layer: LayerUser, Root: filepath.Join(home, ".tether")})
	}
	if workingDir != "" {
		layers = append(layers, LayerSpec{Layer: LayerProject, Root: filepath.Join(Expand(workingDir), ".tether")})
	}
	return layers
}

// LayeredAgent wraps an Agent with its origin metadata so callers (notably
// `mux agents list`) can display which layer the entry came from.
type LayeredAgent struct {
	Agent Agent
	Layer Layer
	Path  string
}

// LayeredCatalog is a discovery view across the three layers. Currently
// surfaces agents + skill/boot-profile paths; per-kind structured loaders
// can layer on top as needed.
type LayeredCatalog struct {
	Agents           map[string]LayeredAgent
	SkillPaths       map[string]LayeredPath // skill id → file
	BootProfilePaths map[string]LayeredPath // profile id (file basename minus .yaml) → file
}

// LayeredPath records a discovered file path and its origin layer.
type LayeredPath struct {
	Path  string
	Layer Layer
}

// Discover walks the layers in order and returns a merged LayeredCatalog with
// later-layer-wins semantics. Missing layer directories are skipped silently.
//
// Within each layer the expected subdirectories are:
//
//	<root>/agents/*.yaml
//	<root>/boot-profiles/*.yaml
//	<root>/skills/*.md
func Discover(layers []LayerSpec) (*LayeredCatalog, error) {
	cat := &LayeredCatalog{
		Agents:           map[string]LayeredAgent{},
		SkillPaths:       map[string]LayeredPath{},
		BootProfilePaths: map[string]LayeredPath{},
	}
	for _, l := range layers {
		if l.Root == "" {
			continue
		}
		if err := discoverAgents(cat, l); err != nil {
			return nil, err
		}
		if err := discoverPathsInto(cat.SkillPaths, l, "skills", ".md"); err != nil {
			return nil, err
		}
		if err := discoverPathsInto(cat.BootProfilePaths, l, "boot-profiles", ".yaml"); err != nil {
			return nil, err
		}
	}
	return cat, nil
}

func discoverAgents(cat *LayeredCatalog, l LayerSpec) error {
	dir := filepath.Join(l.Root, "agents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if ext := filepath.Ext(name); ext != ".yaml" && ext != ".yml" {
			continue
		}
		path := filepath.Join(dir, name)
		var a Agent
		if err := loadYAML(path, &a); err != nil {
			return fmt.Errorf("layer %s agent %s: %w", l.Layer, name, err)
		}
		if a.ID == "" {
			return fmt.Errorf("layer %s agent %s: missing id", l.Layer, name)
		}
		cat.Agents[a.ID] = LayeredAgent{Agent: a, Layer: l.Layer, Path: path}
	}
	return nil
}

func discoverPathsInto(target map[string]LayeredPath, l LayerSpec, subdir, ext string) error {
	dir := filepath.Join(l.Root, subdir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if filepath.Ext(name) != ext {
			continue
		}
		id := name[:len(name)-len(ext)]
		target[id] = LayeredPath{Path: filepath.Join(dir, name), Layer: l.Layer}
	}
	return nil
}
