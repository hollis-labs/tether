package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// projectLayer is one registered project's authority-bearing layer.
type projectLayer struct {
	id       string
	repoRoot string // the project's repository, expanded
	root     string // repoRoot/.tether
}

// projectLayers lists the registered projects that have a repo_root, by id.
func projectLayers(cat *Catalog) []projectLayer {
	if cat == nil {
		return nil
	}
	ids := []string{}
	for id := range cat.Projects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []projectLayer
	for _, id := range ids {
		if root := cat.Projects[id].RepoRoot; root != "" {
			repo := Expand(root)
			out = append(out, projectLayer{id: id, repoRoot: repo, root: filepath.Join(repo, ".tether")})
		}
	}
	return out
}

// userLayer is ~/.tether, when the home directory is known.
func userLayer() (LayerSpec, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return LayerSpec{}, false
	}
	return LayerSpec{Layer: LayerUser, Root: filepath.Join(home, ".tether")}, true
}

// LayeredCatalogLayers is shared by loading and confinement: every user and
// registered project's layer is authority-bearing, including an empty layer.
func LayeredCatalogLayers(cat *Catalog) []LayerSpec {
	layers := []LayerSpec{}
	if user, ok := userLayer(); ok {
		layers = append(layers, user)
	}
	for _, p := range projectLayers(cat) {
		layers = append(layers, LayerSpec{Layer: LayerProject, Root: p.root})
	}
	return layers
}

// SkippedProjectLayer is a registered project whose layer protection left out
// because its repo_root cannot be used on this host (it does not exist, or is
// not a directory). Nothing exists there to protect; see PrepareCatalogProtection
// for what that does and does not cover.
type SkippedProjectLayer struct {
	Project  string `json:"project"`
	RepoRoot string `json:"repo_root"`
	Reason   string `json:"reason"`
}

// ProjectRootError is returned when the project a launch is for has a repo_root
// that cannot be used: its own layer cannot be protected, and neither can the
// workspace the launch would run in. It names the project and the path so the
// caller can say which catalog entry to fix.
type ProjectRootError struct {
	Project string
	Root    string
	Reason  string
}

func (e *ProjectRootError) Error() string {
	return fmt.Sprintf("project %q: its repo_root %s %s, so a launch for it cannot be protected: restore the directory, or point the project at one that exists", e.Project, e.Root, e.Reason)
}

// CatalogProtection is what PrepareCatalogProtection decided.
type CatalogProtection struct {
	// Dirs are the real paths to bind read-only, in a stable order.
	Dirs []string
	// Skipped are registered projects left out because their repo_root cannot be
	// used. They are reported, never silent.
	Skipped []SkippedProjectLayer
	// Created are the empty layer directories this call had to create so that no
	// child can create one first (see PrepareCatalogProtection).
	Created []string
}

// CatalogProtectionDirs is PrepareCatalogProtection for a caller with no launch
// of its own (a daemon upstream, a health probe): any project with an unusable
// repo_root is skipped.
func CatalogProtectionDirs(catalogRoot string, cat *Catalog) ([]string, error) {
	p, err := PrepareCatalogProtection(catalogRoot, cat, "")
	if err != nil {
		return nil, err
	}
	return p.Dirs, nil
}

// PrepareCatalogProtection prepares read-only mount anchors before an agent or
// daemon upstream can run. Empty layers must exist too, otherwise a child
// could create one and inject configuration into the next LoadLayered: that
// reads EVERY registered project's layer, not only the launching project's, so
// every existing project root gets its (empty) .tether created here, once, and
// restricting the creation to the launching project would leave the others open
// to a planted layer. A layer that already exists is protected as it is.
//
// A project whose repo_root does not exist, or is not a directory, has no layer
// to anchor and nothing to create (Tether never creates a missing repository).
// It used to abort the whole call, so one dead catalog entry made every
// protected launch fail. Now:
//
//   - launching names the project the launch is for. If that project's root is
//     unusable the launch cannot be protected or run: a *ProjectRootError names
//     the project and path, before anything is created.
//   - any other such project is skipped and listed in Skipped. That leaves a
//     residue to know about: while its root is missing, a protected agent could
//     create the directory and a .tether inside it, and the next catalog load
//     would read that layer for that project. Skipped is surfaced (health,
//     doctor, a warning) so the dead entry gets fixed rather than forgotten.
//
// Every project root is checked before any directory is created, so a call that
// returns an error has no side effects.
func PrepareCatalogProtection(catalogRoot string, cat *Catalog, launching string) (CatalogProtection, error) {
	var out CatalogProtection
	projects := projectLayers(cat)
	skipped := map[string]bool{}
	for _, p := range projects {
		if !filepath.IsAbs(p.repoRoot) || filepath.Clean(p.repoRoot) == string(filepath.Separator) {
			return CatalogProtection{}, fmt.Errorf("protect catalog layer: project %q: repo_root %q must be an absolute directory other than /", p.id, p.repoRoot)
		}
		reason, err := projectRootProblem(p.repoRoot)
		if err != nil {
			return CatalogProtection{}, fmt.Errorf("protect catalog layer: project %q: %w", p.id, err)
		}
		if reason == "" {
			continue
		}
		if p.id == launching {
			return CatalogProtection{}, &ProjectRootError{Project: p.id, Root: p.repoRoot, Reason: reason}
		}
		skipped[p.id] = true
		out.Skipped = append(out.Skipped, SkippedProjectLayer{Project: p.id, RepoRoot: p.repoRoot, Reason: reason})
	}

	roots := []string{Expand(catalogRoot)}
	if user, ok := userLayer(); ok {
		roots = append(roots, user.Root)
	}
	for _, p := range projects {
		if !skipped[p.id] {
			roots = append(roots, p.root)
		}
	}
	if cat != nil {
		r := cat.Global.Catalog.Roots
		for _, root := range []struct{ configured, fallback string }{{r.Projects, "projects"}, {r.Agents, "agents"}, {r.Providers, "providers"}, {r.Launches, "launches"}} {
			roots = append(roots, resolveRoot(catalogRoot, root.configured, root.fallback))
		}
	}
	for _, root := range roots {
		if !filepath.IsAbs(root) || filepath.Clean(root) == string(filepath.Separator) {
			return CatalogProtection{}, fmt.Errorf("protect catalog layer: root must be an absolute directory")
		}
		// Do not create a missing project repository or unrelated ancestors.
		if _, err := os.Stat(root); os.IsNotExist(err) {
			if _, parentErr := os.Stat(filepath.Dir(root)); parentErr != nil {
				return CatalogProtection{}, fmt.Errorf("protect catalog layer: parent of %s unavailable: %w", root, parentErr)
			}
			if err := os.Mkdir(root, 0700); err != nil {
				return CatalogProtection{}, fmt.Errorf("prepare catalog layer: %w", err)
			}
			out.Created = append(out.Created, root)
		}
		canonical, err := filepath.EvalSymlinks(root)
		if err != nil {
			return CatalogProtection{}, fmt.Errorf("protect catalog layer: %w", err)
		}
		info, err := os.Stat(canonical)
		if err != nil || !info.IsDir() || canonical == string(filepath.Separator) {
			return CatalogProtection{}, fmt.Errorf("protect catalog layer: root must be a directory")
		}
		covered := false
		for _, existing := range out.Dirs {
			if rel, err := filepath.Rel(existing, canonical); err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))) {
				covered = true
			}
		}
		if !covered {
			out.Dirs = append(out.Dirs, canonical)
		}
	}
	return out, nil
}

// projectRootProblem says why a project's repo_root cannot hold a layer, or ""
// when it can. A path that cannot be examined for another reason (permissions)
// is an error, not a skip: protection that cannot see a root fails closed.
func projectRootProblem(repoRoot string) (string, error) {
	info, err := os.Stat(repoRoot)
	switch {
	case err == nil && info.IsDir():
		return "", nil
	case err == nil:
		return "is not a directory", nil
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return "does not exist", nil
	}
	return "", err
}
