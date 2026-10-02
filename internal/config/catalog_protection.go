package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LayeredCatalogLayers is shared by loading and confinement: every user and
// registered project's layer is authority-bearing, including an empty layer.
func LayeredCatalogLayers(cat *Catalog) []LayerSpec {
	layers := []LayerSpec{}
	if home, err := os.UserHomeDir(); err == nil {
		layers = append(layers, LayerSpec{Layer: LayerUser, Root: filepath.Join(home, ".tether")})
	}
	if cat == nil {
		return layers
	}
	ids := []string{}
	for id := range cat.Projects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if root := cat.Projects[id].RepoRoot; root != "" {
			layers = append(layers, LayerSpec{Layer: LayerProject, Root: filepath.Join(Expand(root), ".tether")})
		}
	}
	return layers
}

// CatalogProtectionDirs prepares read-only mount anchors before an agent or
// daemon upstream can run. Empty layers must exist too, otherwise a child
// could create one and inject configuration into the next LoadLayered.
func CatalogProtectionDirs(catalogRoot string, cat *Catalog) ([]string, error) {
	roots := []string{Expand(catalogRoot)}
	for _, layer := range LayeredCatalogLayers(cat) {
		roots = append(roots, layer.Root)
	}
	if cat != nil {
		r := cat.Global.Catalog.Roots
		for _, root := range []struct{ configured, fallback string }{{r.Projects, "projects"}, {r.Agents, "agents"}, {r.Providers, "providers"}, {r.Launches, "launches"}} {
			roots = append(roots, resolveRoot(catalogRoot, root.configured, root.fallback))
		}
	}
	dirs := []string{}
	for _, root := range roots {
		if !filepath.IsAbs(root) || filepath.Clean(root) == string(filepath.Separator) {
			return nil, fmt.Errorf("protect catalog layer: root must be an absolute directory")
		}
		// Do not create a missing project repository or unrelated ancestors.
		if _, err := os.Stat(root); os.IsNotExist(err) {
			if _, parentErr := os.Stat(filepath.Dir(root)); parentErr != nil {
				return nil, fmt.Errorf("protect catalog layer: parent unavailable: %w", parentErr)
			}
			if err := os.Mkdir(root, 0700); err != nil {
				return nil, fmt.Errorf("prepare catalog layer: %w", err)
			}
		}
		canonical, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, fmt.Errorf("protect catalog layer: %w", err)
		}
		info, err := os.Stat(canonical)
		if err != nil || !info.IsDir() || canonical == string(filepath.Separator) {
			return nil, fmt.Errorf("protect catalog layer: root must be a directory")
		}
		covered := false
		for _, existing := range dirs {
			if rel, err := filepath.Rel(existing, canonical); err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))) {
				covered = true
			}
		}
		if !covered {
			dirs = append(dirs, canonical)
		}
	}
	return dirs, nil
}
