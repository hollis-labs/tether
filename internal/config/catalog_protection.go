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

// SkippedProjectLayer is a registered project whose layer protection left out:
// its repo_root cannot be used on this host (it does not exist, or is not a
// directory) AND no agent could create it either, because the nearest existing
// directory above it is not writable by this user. Nothing can be planted there;
// see PrepareCatalogProtection.
type SkippedProjectLayer struct {
	Project  string `json:"project"`
	RepoRoot string `json:"repo_root"`
	Reason   string `json:"reason"`
}

// CreatedProjectRoot is a registered project whose repo_root did not exist and
// that an agent COULD have created (the nearest existing directory above it is
// writable). Protection created the root with only an empty .tether in it and
// anchored that layer read-only, so an agent cannot plant a layer there. The
// project is still a stale catalog entry to fix.
type CreatedProjectRoot struct {
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

// UnprotectableLayerError is returned when a project's layer cannot be protected
// and cannot be left alone either: its repo_root runs through something that is
// not a directory, in a directory an agent can write to, so the agent could
// replace it with a directory and plant a layer, and Tether will not delete the
// user's file to prevent that. It refuses the launch rather than leave the layer
// open.
type UnprotectableLayerError struct {
	Project string
	Root    string
	Why     string
}

func (e *UnprotectableLayerError) Error() string {
	return fmt.Sprintf("project %q: its layer cannot be protected: its repo_root %s %s: fix or remove the project", e.Project, e.Root, e.Why)
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
	// CreatedRoots are projects whose missing repo_root this call created, empty
	// but for the .tether layer it anchors, because an agent could have.
	CreatedRoots []CreatedProjectRoot
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

// layerPlan is how one project's layer will be protected.
type layerPlan struct {
	project string
	layer   string // the .tether directory to anchor
	chain   bool   // the root is missing: create the whole chain down to layer
	note    CreatedProjectRoot
}

// PrepareCatalogProtection prepares read-only mount anchors before an agent or
// daemon upstream can run. Empty layers must exist too, otherwise a child
// could create one and inject configuration into the next LoadLayered: that
// reads EVERY registered project's layer, not only the launching project's, so
// every project gets its (empty) .tether created here, once, and restricting the
// creation to the launching project would leave the others open to a planted
// layer. A layer that already exists is protected as it is.
//
// A project whose repo_root does not exist, or is not a directory, used to abort
// the whole call, so one dead catalog entry made every protected launch fail.
// But skipping it is not safe either: the sandbox is the whole host, writable,
// with only Tether's own directories bound read-only, so an agent can create a
// missing root and a .tether in it, and the next catalog load reads that layer
// for that project (a planted agent with bypass permissions and arbitrary args,
// reproduced). So what happens depends on whether an agent could create it:
//
//   - launching names the project the launch is for. If that project's root is
//     unusable the launch cannot be protected or run: a *ProjectRootError names
//     the project and path. Its root is never created.
//   - any other project whose root is unusable: the nearest existing directory
//     above where the root would land (following a dangling symlink) is tested
//     with access(2) as this user, who is who the agent runs as. If it is
//     writable, the root is created holding only .tether (0700) and that layer is
//     anchored like any other (CreatedRoots, and Created); the project is still
//     a stale entry to fix, so this is reported. If it is not writable, the agent
//     cannot create the root either, there is nothing to plant into, and the
//     project is skipped (Skipped).
//   - a file in the way (a root that is a file, or runs through one) under a
//     writable directory: an agent could replace the file with a directory, and a
//     user's file is not Tether's to delete, so the call fails closed with a
//     *UnprotectableLayerError naming the project.
//
// Every project root is examined, and every one of those outcomes decided,
// before any directory is created, so a call that returns an error because of a
// project has no side effects.
func PrepareCatalogProtection(catalogRoot string, cat *Catalog, launching string) (CatalogProtection, error) {
	var out CatalogProtection
	projects := projectLayers(cat)
	plans := map[string]layerPlan{}
	for _, p := range projects {
		if !filepath.IsAbs(p.repoRoot) || filepath.Clean(p.repoRoot) == string(filepath.Separator) {
			return CatalogProtection{}, fmt.Errorf("protect catalog layer: project %q: repo_root %q must be an absolute directory other than /", p.id, p.repoRoot)
		}
		reason, err := projectRootProblem(p.repoRoot)
		if err != nil {
			return CatalogProtection{}, fmt.Errorf("protect catalog layer: project %q: %w", p.id, err)
		}
		if reason == "" {
			plans[p.id] = layerPlan{project: p.id, layer: p.root}
			continue
		}
		if p.id == launching {
			return CatalogProtection{}, &ProjectRootError{Project: p.id, Root: p.repoRoot, Reason: reason}
		}
		target := RealPath(p.repoRoot) // where an agent's mkdir would land
		ancestor, blocked, err := nearestExistingDir(target)
		if err != nil {
			return CatalogProtection{}, fmt.Errorf("protect catalog layer: project %q: %w", p.id, err)
		}
		switch {
		case !agentCanWrite(ancestor):
			out.Skipped = append(out.Skipped, SkippedProjectLayer{Project: p.id, RepoRoot: p.repoRoot,
				Reason: fmt.Sprintf("%s, and an agent cannot create it either (%s is not writable)", reason, ancestor)})
		case blocked:
			return CatalogProtection{}, &UnprotectableLayerError{Project: p.id, Root: p.repoRoot,
				Why: fmt.Sprintf("%s: a file is in the way inside %s, which an agent can write to, so it could replace the file with a directory and plant a layer", reason, ancestor)}
		default:
			plans[p.id] = layerPlan{project: p.id, layer: filepath.Join(target, ".tether"), chain: true, note: CreatedProjectRoot{
				Project: p.id, RepoRoot: p.repoRoot,
				Reason: fmt.Sprintf("%s and an agent could have created it (%s is writable): it was created holding only an empty .tether, anchored read-only, so a protected agent cannot plant a layer there", reason, ancestor)}}
		}
	}

	roots := []string{Expand(catalogRoot)}
	if user, ok := userLayer(); ok {
		roots = append(roots, user.Root)
	}
	chain := map[string]CreatedProjectRoot{}
	for _, p := range projects {
		if plan, ok := plans[p.id]; ok {
			roots = append(roots, plan.layer)
			if plan.chain {
				chain[plan.layer] = plan.note
			}
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
		if note, missingRoot := chain[root]; missingRoot {
			// The project's own root is missing: create the chain down to its
			// layer. Only here: a repository is never created otherwise.
			if err := os.MkdirAll(root, 0700); err != nil {
				return CatalogProtection{}, fmt.Errorf("prepare catalog layer: %w", err)
			}
			out.Created = append(out.Created, root)
			out.CreatedRoots = append(out.CreatedRoots, note)
		} else if _, err := os.Stat(root); os.IsNotExist(err) {
			// Do not create a missing project repository or unrelated ancestors.
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

// nearestExistingDir finds, for a path that may not exist, the nearest directory
// that does: the place an agent's mkdir -p would start creating. blocked is true
// when something that is not a directory sits in the path first: the directory
// returned is then the one that holds it, whose write permission decides whether
// an agent could replace it. A path that cannot be examined for another reason
// is an error: protection that cannot see fails closed.
func nearestExistingDir(path string) (dir string, blocked bool, err error) {
	for cur := path; ; {
		info, statErr := os.Stat(cur)
		switch {
		case statErr == nil && info.IsDir():
			return cur, false, nil
		case statErr == nil:
			return filepath.Dir(cur), true, nil
		case !errors.Is(statErr, fs.ErrNotExist) && !errors.Is(statErr, syscall.ENOTDIR):
			return "", false, statErr
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return cur, false, nil
		}
		cur = parent
	}
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
