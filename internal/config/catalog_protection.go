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
// directory) AND no agent could create it either: the agent runs as this user, and
// nothing from the nearest existing directory above the root up to / is writable
// by this user or owned by this user (who could make it writable, or move it
// aside). Nothing can be planted there; see PrepareCatalogProtection.
type SkippedProjectLayer struct {
	Project  string `json:"project"`
	RepoRoot string `json:"repo_root"`
	Reason   string `json:"reason"`
}

// CreatedProjectRoot is a registered project whose repo_root did not exist and
// that an agent COULD have created (the nearest existing directory above it is
// writable). Protection created the root with only a .tether in it (holding
// PlaceholderMarker) and anchored that layer read-only, so an agent cannot plant
// a layer there. It is reported on every call while the placeholder is all there
// is, not only the call that created it. The project is still a stale catalog
// entry to fix.
type CreatedProjectRoot struct {
	Project  string `json:"project"`
	RepoRoot string `json:"repo_root"`
	Reason   string `json:"reason"`
}

// PlaceholderMarker is the one file protection writes inside the .tether layer of
// a project root it had to create. It is how a placeholder is told from a real
// project directory that happens to hold nothing yet: after a restart, or when
// another project's launch created the root, nothing else says that the root is
// a stand-in for a missing repository. The loader reads only <layer>/<kind>/*.yaml,
// so it ignores this file.
const PlaceholderMarker = "created-by-tether-protection"

const placeholderNote = `Tether created this directory because the project's repo_root did not exist.
A protected agent runs in a sandbox where the whole host is writable, so it could
have created a .tether here and planted a catalog layer; this read-only .tether
closes that. The directory holds nothing else. To use the project, remove this
directory and restore the repository, or point the project at one that exists.
`

// placeholderRoot reports whether root is exactly what protection leaves for a
// missing project root: a directory holding only .tether, which holds only the
// marker. A project directory that merely holds an empty .tether is not one: that
// is what protection leaves in any project root that exists.
func placeholderRoot(root string) bool {
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".tether" || !entries[0].IsDir() {
		return false
	}
	layer, err := os.ReadDir(filepath.Join(root, ".tether"))
	return err == nil && len(layer) == 1 && layer[0].Name() == PlaceholderMarker && layer[0].Type().IsRegular()
}

// ProjectRootError is returned when the project a launch is for has a repo_root
// that cannot be used: it is missing, is not a directory, or is only the
// placeholder protection created for it. The launch cannot run in it. It names the
// project and the path so the caller can say which catalog entry to fix.
type ProjectRootError struct {
	Project string
	Root    string
	Reason  string
}

func (e *ProjectRootError) Error() string {
	return fmt.Sprintf("project %q: its repo_root %s %s, so a launch for it cannot run: restore the directory, or point the project at one that exists", e.Project, e.Root, e.Reason)
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
	// CreatedRoots are the projects whose repo_root is a placeholder: this call
	// created it, empty but for the .tether layer it anchors, because an agent
	// could have; or an earlier call did, and it is still all there is.
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
//     unusable, or is only a placeholder protection created for it earlier, the
//     launch cannot run: a *ProjectRootError names the project and path. Its
//     root is never created by its own launch. Without the placeholder rule the
//     refusal would be unreachable in practice: the first protected call of the
//     daemon creates every other project's root, this one's included.
//   - any other project whose root is unusable: the nearest existing directory
//     above where the root would land (following a dangling symlink) is tested
//     with access(2) as this user, who is who the agent runs as. If it is
//     writable, the root is created holding only .tether (0700, with
//     PlaceholderMarker in it) and that layer is anchored like any other
//     (CreatedRoots, and Created); the project is still
//     a stale entry to fix, so this is reported.
//   - if it is not writable, that is not the end of it, because the agent is this
//     user: a directory this user owns can be made writable (chmod), and a
//     directory can be moved aside, with a directory of the agent's own put in its
//     place, by anyone who can write the directory above it (a sticky directory
//     only lets a user move what that user owns, which is covered by the first).
//     So the whole chain from that directory up to / is examined, and the project
//     is skipped (Skipped) only when no directory in it is owned by this user or
//     writable by it: then nothing the agent can do creates the root.
//   - otherwise (not writable, but the agent can get past it) the root cannot be
//     anchored either: creating it needs a permission Tether will not take by
//     changing the user's directory. The call fails closed with a
//     *UnprotectableLayerError naming the project and the way past.
//   - a file in the way (a root that is a file, or runs through one): the same
//     test on the directory that holds it. Where an agent can write to it, or get
//     past it, the agent could replace the file with a directory and plant a
//     layer, and a user's file is not Tether's to delete, so the call fails closed
//     with a *UnprotectableLayerError naming the project; otherwise it is skipped.
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
			if placeholderRoot(p.repoRoot) {
				if p.id == launching {
					return CatalogProtection{}, &ProjectRootError{Project: p.id, Root: p.repoRoot,
						Reason: "is only the placeholder Tether created when it was missing (it holds nothing but .tether/" + PlaceholderMarker + "): remove it and restore the repository"}
				}
				out.CreatedRoots = append(out.CreatedRoots, CreatedProjectRoot{Project: p.id, RepoRoot: p.repoRoot,
					Reason: "is still the placeholder Tether created when it was missing: it holds only a read-only .tether, so a protected agent cannot plant a layer there"})
			}
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
		canWrite := agentCanWrite(ancestor)
		past := ""
		if !canWrite {
			if past, err = agentGetsPast(ancestor); err != nil {
				return CatalogProtection{}, fmt.Errorf("protect catalog layer: project %q: %w", p.id, err)
			}
		}
		switch {
		case !canWrite && past == "":
			out.Skipped = append(out.Skipped, SkippedProjectLayer{Project: p.id, RepoRoot: p.repoRoot,
				Reason: fmt.Sprintf("%s, and an agent cannot create it either (%s is not writable, and no directory from it up to / is owned by or writable by this user)", reason, ancestor)})
		case !canWrite:
			return CatalogProtection{}, &UnprotectableLayerError{Project: p.id, Root: p.repoRoot,
				Why: fmt.Sprintf("%s: %s is not writable, but an agent runs as this user and can get past that (%s), so the layer cannot be left open, and cannot be anchored without changing the permissions of a directory that is not Tether's", reason, ancestor, past)}
		case blocked:
			return CatalogProtection{}, &UnprotectableLayerError{Project: p.id, Root: p.repoRoot,
				Why: fmt.Sprintf("%s: a file is in the way inside %s, which an agent can write to, so it could replace the file with a directory and plant a layer", reason, ancestor)}
		default:
			plans[p.id] = layerPlan{project: p.id, layer: filepath.Join(target, ".tether"), chain: true, note: CreatedProjectRoot{
				Project: p.id, RepoRoot: p.repoRoot,
				Reason: fmt.Sprintf("%s and an agent could have created it (%s is writable): it was created holding only a read-only .tether, so a protected agent cannot plant a layer there", reason, ancestor)}}
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
			if err := os.WriteFile(filepath.Join(root, PlaceholderMarker), []byte(placeholderNote), 0600); err != nil {
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

// dirFacts is what decides whether an agent running as this user can get past a
// directory, collected for each directory from the nearest existing ancestor of a
// missing project root up to /.
type dirFacts struct {
	path     string
	uid      uint32 // owner
	sticky   bool   // sticky bit: only an entry's owner (or the directory's) may move it
	writable bool   // access(2) W_OK|X_OK as this user
}

// bypassVia says how an agent running as this user could create entries in
// chain[0] (the nearest existing ancestor, which access(2) says is not writable),
// or "" when nothing it can do gets past. chain runs from that directory up to /;
// euid is the agent's user.
//
// access(2) alone is the wrong test, because the agent is the same user as the
// daemon: a directory this user owns can be made writable with chmod, and any
// directory in the chain can be moved aside and replaced (mv ro ro-old; mkdir ro)
// by someone who can write the directory above it.
func bypassVia(chain []dirFacts, euid uint32) string {
	for i, d := range chain {
		if d.uid == euid {
			if i == 0 {
				return fmt.Sprintf("%s is owned by this user, who can make it writable (chmod) and create the root in it", d.path)
			}
			return fmt.Sprintf("%s, above it, is owned by this user, who can make it writable (chmod) and move %s aside to put a directory of their own in its place", d.path, chain[i-1].path)
		}
		if !d.writable {
			continue
		}
		if i == 0 {
			return fmt.Sprintf("%s is writable by this user", d.path)
		}
		// A directory above the first is a way past when it can be written, unless it
		// is sticky: a sticky directory lets a user move only what that user owns,
		// and an entry this user owns was already caught above.
		if d.sticky {
			continue
		}
		return fmt.Sprintf("%s, above it, is writable by this user, who can rename %s out of the way and put a directory of their own in its place", d.path, chain[i-1].path)
	}
	return ""
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
