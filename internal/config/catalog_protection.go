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
// so it ignores this file. A root is a placeholder while this file is in its
// .tether, whatever else has been put in the root since: restoring the repository
// is done by removing the marker (or the whole placeholder directory).
const PlaceholderMarker = "created-by-tether-protection"

const placeholderNote = `Tether created this directory because the project's repo_root did not exist.
A protected agent runs in a sandbox where the whole host is writable, so it could
have created a .tether here and planted a catalog layer; this read-only .tether
closes that. To use the project, restore the repository and remove this file (or
remove this whole directory first and then restore the repository), or point the
project at one that exists.
`

// placeholderState reports whether root is a placeholder protection created for a
// missing project root, and whether anything besides .tether has been put in it
// since. It is keyed on the marker file inside .tether, nothing else: an agent can
// drop files into the root (only .tether is anchored), and that must not turn a
// placeholder back into a project that launches. A project directory that merely
// holds an empty .tether, which is what protection leaves in every project root
// that exists, is not one.
func placeholderState(root string) (placeholder, extra bool) {
	layer := filepath.Join(root, ".tether")
	if fi, err := os.Lstat(layer); err != nil || !fi.IsDir() {
		return false, false
	}
	if fi, err := os.Lstat(filepath.Join(layer, PlaceholderMarker)); err != nil || !fi.Mode().IsRegular() {
		return false, false
	}
	entries, err := os.ReadDir(root)
	return true, err == nil && len(entries) > 1
}

// placeholderRoot reports whether root is a placeholder (see placeholderState).
func placeholderRoot(root string) bool {
	ok, _ := placeholderState(root)
	return ok
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
// and cannot be left open either, and the cause is that project's catalog entry or
// the file system under it, not the host: its repo_root runs through a file in a
// directory an agent can write to (it could replace the file with a directory and
// plant a layer, and Tether will not delete the user's file to prevent that),
// through a symlink an agent can replace, or cannot be examined at all; or its layer
// turned up under Tether's feet while it was being created. Strict callers (a
// launch of an agent Tether protects) are refused with it; tolerant callers (the
// daemon's MCP gateway, the Codex proxy) leave that one layer out and report it
// (see ProtectionOptions).
type UnprotectableLayerError struct {
	Project string
	Root    string
	Why     string
}

func (e *UnprotectableLayerError) Error() string {
	return fmt.Sprintf("project %q: its layer cannot be protected: its repo_root %s %s: fix or remove the project", e.Project, e.Root, e.Why)
}

// AnchoredAncestor is a project whose repo_root does not exist and whose nearest
// existing directory above it is not writable, but that an agent can get past
// because it runs as the user who owns it (chmod), or can write the directory above
// it (rename it aside). Protection anchors that directory read-only: inside the
// sandbox it cannot be made writable, written to or renamed (it is a mount point, as
// is every directory above it), so the root cannot be created under it.
type AnchoredAncestor struct {
	Project  string `json:"project"`
	RepoRoot string `json:"repo_root"`
	Ancestor string `json:"ancestor"`
	Reason   string `json:"reason"`
}

// UnprotectedProjectLayer is a project whose layer a TOLERANT call (see
// ProtectionOptions) left out because it cannot be protected (an
// UnprotectableLayerError a strict call would have returned). The layer is open to
// whatever that call confines, and it is reported, never silent.
type UnprotectedProjectLayer struct {
	Project  string `json:"project"`
	RepoRoot string `json:"repo_root"`
	Why      string `json:"why"`
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
	// AnchoredAncestors are the unwritable directories this call anchored read-only
	// because an agent could get past them to create a missing project root.
	AnchoredAncestors []AnchoredAncestor
	// Unprotected are the layers a tolerant call left out (see
	// UnprotectedProjectLayer). Always empty for a strict call, which fails instead.
	Unprotected []UnprotectedProjectLayer
}

// ProtectionOptions says who is asking.
type ProtectionOptions struct {
	// Launching names the project the launch is for, if any.
	Launching string
	// Tolerate is for a caller that must keep working while a project's catalog
	// entry is broken, because it is not itself the agent being protected: the
	// daemon's MCP gateway, and the planted proxy of a Codex launch (Codex runs as it
	// did before protection existed). A project whose layer cannot be protected (an
	// *UnprotectableLayerError for a strict call) is left out and reported in
	// Unprotected instead of failing the call. A catalog problem must never take
	// out the gateway or an unprotected Codex launch. A strict call (the launch of
	// an agent Tether protects) still fails closed: leaving a layer open to that
	// agent is the thing protection exists to prevent. The launching project's own
	// missing root is a *ProjectRootError either way, and anything that is not
	// about one project (the catalog root, the user layer) is an error either way.
	Tolerate bool
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
	project  string
	repoRoot string
	layer    string // the .tether directory to anchor
	chain    bool   // the root is missing: create the whole chain down to layer
	note     CreatedProjectRoot
}

// decision is what protection settled for one project, before anything is created.
type decision struct {
	plan        *layerPlan
	skipped     *SkippedProjectLayer
	anchored    *AnchoredAncestor
	placeholder *CreatedProjectRoot // a placeholder that is already there
}

// testHooks let a test do what a racing agent would, between the checks and the
// creation. Nil in production.
var (
	testHookBeforeLayerCreate func(layer string)
	testHookAfterLayerCreate  func(layer string)
)

// PrepareCatalogProtection prepares read-only mount anchors before an agent or
// daemon upstream can run, strictly: see PrepareCatalogProtectionWith.
func PrepareCatalogProtection(catalogRoot string, cat *Catalog, launching string) (CatalogProtection, error) {
	return PrepareCatalogProtectionWith(catalogRoot, cat, ProtectionOptions{Launching: launching})
}

// PrepareCatalogProtectionWith prepares read-only mount anchors before an agent or
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
//   - Launching names the project the launch is for. If that project's root is
//     unusable, or is a placeholder protection created for it earlier (it still
//     holds the marker), the launch cannot run: a *ProjectRootError names the
//     project and path. Its root is never created by its own launch. Without the
//     placeholder rule the refusal would be unreachable in practice: the first
//     protected call of the daemon creates every other project's root, this
//     one's included.
//   - any other project whose root is unusable: the nearest existing directory
//     above where the root would land (following a dangling symlink) is tested
//     with access(2) as this user, who is who the agent runs as. If it is
//     writable, the root is created holding only .tether (0700, with
//     PlaceholderMarker in it) and that layer is anchored like any other
//     (CreatedRoots, and Created); the project is still a stale entry to fix, so
//     this is reported. The layer is made with Mkdir, not MkdirAll, and the call
//     fails if something else made it first or put anything in it, because a layer
//     that turned up under Tether's feet is not Tether's to trust.
//   - if it is not writable, that is not the end of it, because the agent is this
//     user: a directory this user owns can be made writable (chmod), and a
//     directory can be moved aside, with a directory of the agent's own put in its
//     place, by anyone who can write the directory above it (a sticky directory
//     only lets a user move what that user owns, which is covered by the first).
//     So the whole chain from that directory up to / is examined, and the project
//     is skipped (Skipped) only when no directory in it is owned by this user or
//     writable by it: then nothing the agent can do creates the root.
//   - otherwise (not writable, but the agent can get past it) the nearest existing
//     directory is anchored read-only itself (AnchoredAncestors): inside the
//     sandbox it cannot be made writable, written or renamed, and neither can any
//     directory above it (each is a mount point), so the root cannot be created
//     under it. This needs no permission Tether does not have, and it does not
//     refuse the launch of every other project; it does make that directory
//     read-only for the agent, so a launch whose own directories lie inside it is
//     refused (launch.ErrLaunchInsideProtectedPath), as for any anchor.
//   - a file in the way (a root that is a file, or runs through one): the same
//     test on the directory that holds it. Where an agent can write to it, it could
//     replace the file with a directory and plant a layer, and a user's file is not
//     Tether's to delete, so the call fails closed with a *UnprotectableLayerError
//     naming the project; where it cannot but can get past it, that directory is
//     anchored like any other; otherwise the project is skipped.
//   - a repo_root that runs through a symlink, in a directory an agent can write
//     or get past: the agent can unlink the link and put a real directory with a
//     planted layer where it was, and the loader reads the layer through the link.
//     An anchor on the link's target does not pin the link, so the call fails
//     closed with a *UnprotectableLayerError saying to point the project at the
//     real path. A link out of the agent's reach is followed.
//
// A project whose root cannot be examined (permissions, a symlink loop) is an
// *UnprotectableLayerError too, not a bare internal error. A root with a name too
// long to exist is skipped: nobody can create it.
//
// Every project root is examined, and every one of those outcomes decided,
// before any directory is created, so a call that returns an error because of a
// project has no side effects. (A layer found planted while it is being created
// is the one error that can come after earlier placeholders were made; those are
// idempotent and stay.)
//
// With opts.Tolerate, a project that would be an *UnprotectableLayerError is left
// out and listed in Unprotected instead.
func PrepareCatalogProtectionWith(catalogRoot string, cat *Catalog, opts ProtectionOptions) (CatalogProtection, error) {
	var out CatalogProtection
	projects := projectLayers(cat)
	plans := map[string]layerPlan{}
	var anchors []string // unwritable ancestors to anchor, real paths
	// tolerated reports whether err is one a tolerant call leaves out and reports.
	tolerated := func(project, repoRoot string, err error) bool {
		var layerErr *UnprotectableLayerError
		if !opts.Tolerate || !errors.As(err, &layerErr) {
			return false
		}
		out.Unprotected = append(out.Unprotected, UnprotectedProjectLayer{Project: project, RepoRoot: repoRoot, Why: layerErr.Why})
		return true
	}
	for _, p := range projects {
		d, err := decideProject(p, opts.Launching)
		if err != nil {
			if tolerated(p.id, p.repoRoot, err) {
				continue
			}
			return CatalogProtection{}, err
		}
		if d.plan != nil {
			plans[p.id] = *d.plan
		}
		if d.skipped != nil {
			out.Skipped = append(out.Skipped, *d.skipped)
		}
		if d.anchored != nil {
			out.AnchoredAncestors = append(out.AnchoredAncestors, *d.anchored)
			anchors = append(anchors, d.anchored.Ancestor)
		}
		if d.placeholder != nil {
			out.CreatedRoots = append(out.CreatedRoots, *d.placeholder)
		}
	}

	roots := []string{Expand(catalogRoot)}
	if user, ok := userLayer(); ok {
		roots = append(roots, user.Root)
	}
	chain := map[string]layerPlan{}
	for _, p := range projects {
		if plan, ok := plans[p.id]; ok {
			roots = append(roots, plan.layer)
			if plan.chain {
				chain[plan.layer] = plan
			}
		}
	}
	roots = append(roots, anchors...)
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
		if plan, missingRoot := chain[root]; missingRoot {
			// The project's own root is missing: create the chain down to its
			// layer. Only here: a repository is never created otherwise.
			if err := createPlaceholder(plan); err != nil {
				if tolerated(plan.project, plan.repoRoot, err) {
					continue
				}
				return CatalogProtection{}, err
			}
			out.Created = append(out.Created, root)
			out.CreatedRoots = append(out.CreatedRoots, plan.note)
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

// unprotectable is the error for a project whose layer cannot be protected.
func unprotectable(p projectLayer, format string, args ...any) error {
	return &UnprotectableLayerError{Project: p.id, Root: p.repoRoot, Why: fmt.Sprintf(format, args...)}
}

// decideProject settles how one project's layer is protected, creating nothing.
func decideProject(p projectLayer, launching string) (decision, error) {
	if !filepath.IsAbs(p.repoRoot) || filepath.Clean(p.repoRoot) == string(filepath.Separator) {
		return decision{}, unprotectable(p, "must be an absolute directory other than /")
	}
	reason, uncreatable, err := projectRootProblem(p.repoRoot)
	if err != nil {
		return decision{}, unprotectable(p, "cannot be examined: %v", err)
	}
	if reason == "" {
		var d decision
		if placeholder, extra := placeholderState(p.repoRoot); placeholder {
			if p.id == launching {
				return decision{}, &ProjectRootError{Project: p.id, Root: p.repoRoot,
					Reason: "is a placeholder Tether created when it was missing (it still holds Tether's marker .tether/" + PlaceholderMarker + "): restore the repository and remove that marker, or remove the placeholder directory and restore the repository"}
			}
			why := "is still the placeholder Tether created when it was missing: its .tether is read-only, so a protected agent cannot plant a layer there"
			if extra {
				why += " (something else has been put in the directory since)"
			}
			d.placeholder = &CreatedProjectRoot{Project: p.id, RepoRoot: p.repoRoot, Reason: why}
		}
		if link, err := replaceableSymlink(p.repoRoot); err != nil {
			return decision{}, unprotectable(p, "cannot be examined: %v", err)
		} else if link != "" {
			return decision{}, unprotectable(p, "runs through a symlink an agent can replace (%s): it could put a real directory with a planted layer where the link is, and protection cannot pin a symlink: point the project at the real path", link)
		}
		d.plan = &layerPlan{project: p.id, repoRoot: p.repoRoot, layer: p.root}
		return d, nil
	}
	if p.id == launching {
		return decision{}, &ProjectRootError{Project: p.id, Root: p.repoRoot, Reason: reason}
	}
	if uncreatable {
		return decision{skipped: &SkippedProjectLayer{Project: p.id, RepoRoot: p.repoRoot,
			Reason: reason + ", so nobody can create it"}}, nil
	}
	if link, err := replaceableSymlink(p.repoRoot); err != nil {
		return decision{}, unprotectable(p, "cannot be examined: %v", err)
	} else if link != "" {
		return decision{}, unprotectable(p, "runs through a symlink an agent can replace (%s): it could unlink it and create the root itself, and protection cannot pin a symlink: point the project at the real path", link)
	}
	target := RealPath(p.repoRoot) // where an agent's mkdir would land
	ancestor, blocked, err := nearestExistingDir(target)
	if err != nil {
		return decision{}, unprotectable(p, "cannot be examined: %v", err)
	}
	canWrite := agentCanWrite(ancestor)
	past := ""
	if !canWrite {
		if past, err = agentGetsPast(ancestor); err != nil {
			return decision{}, unprotectable(p, "cannot be examined: %v", err)
		}
	}
	switch {
	case !canWrite && past == "":
		return decision{skipped: &SkippedProjectLayer{Project: p.id, RepoRoot: p.repoRoot,
			Reason: fmt.Sprintf("%s, and an agent cannot create it either (%s is not writable, and no directory from it up to / is owned by or writable by this user)", reason, ancestor)}}, nil
	case !canWrite:
		resolved, err := filepath.EvalSymlinks(ancestor)
		if err != nil {
			return decision{}, unprotectable(p, "cannot be examined: %v", err)
		}
		return decision{anchored: &AnchoredAncestor{Project: p.id, RepoRoot: p.repoRoot, Ancestor: resolved,
			Reason: fmt.Sprintf("%s: %s is not writable, but an agent runs as this user and can get past that (%s), so %s is anchored read-only: inside the sandbox it cannot be made writable, written to or moved aside, so nothing can be planted under it", reason, ancestor, past, resolved)}}, nil
	case blocked:
		return decision{}, unprotectable(p, "%s: a file is in the way inside %s, which an agent can write to, so it could replace the file with a directory and plant a layer", reason, ancestor)
	}
	return decision{plan: &layerPlan{project: p.id, repoRoot: p.repoRoot, layer: filepath.Join(target, ".tether"), chain: true, note: CreatedProjectRoot{
		Project: p.id, RepoRoot: p.repoRoot,
		Reason: fmt.Sprintf("%s and an agent could have created it (%s is writable): it was created holding only a read-only .tether, so a protected agent cannot plant a layer there", reason, ancestor)}}}, nil
}

// createPlaceholder creates a missing project root as a placeholder: the root, then
// .tether inside it with Mkdir (so that it fails if something made it first), then
// the marker, then a check that the layer holds the marker and nothing else. A layer
// that appeared before Tether made it, or got something in it while Tether made it,
// is a planted layer: the call fails with an *UnprotectableLayerError instead of
// anchoring it and calling it clean.
func createPlaceholder(plan layerPlan) error {
	cannot := func(err error) error {
		return &UnprotectableLayerError{Project: plan.project, Root: plan.repoRoot,
			Why: fmt.Sprintf("could not be created as a placeholder (%v), so the layer cannot be anchored", err)}
	}
	if err := os.MkdirAll(filepath.Dir(plan.layer), 0700); err != nil {
		return cannot(err)
	}
	if testHookBeforeLayerCreate != nil {
		testHookBeforeLayerCreate(plan.layer)
	}
	if err := os.Mkdir(plan.layer, 0700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return &UnprotectableLayerError{Project: plan.project, Root: plan.repoRoot,
				Why: fmt.Sprintf("had its layer %s created by something else while protection was creating it: a layer that turned up like that is not Tether's to trust, and was left as it is", plan.layer)}
		}
		return cannot(err)
	}
	if testHookAfterLayerCreate != nil {
		testHookAfterLayerCreate(plan.layer)
	}
	marker := filepath.Join(plan.layer, PlaceholderMarker)
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600) //nolint:gosec // G304: a fixed name inside the layer Tether just made
	if err == nil {
		_, werr := f.WriteString(placeholderNote)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		err = werr
	}
	if err != nil {
		return cannot(err)
	}
	entries, err := os.ReadDir(plan.layer)
	if err != nil || len(entries) != 1 || entries[0].Name() != PlaceholderMarker || !entries[0].Type().IsRegular() {
		return &UnprotectableLayerError{Project: plan.project, Root: plan.repoRoot,
			Why: fmt.Sprintf("had something put in its layer %s while protection was creating it (it holds %d entries, not just Tether's marker): it is not Tether's to trust, and was left as it is", plan.layer, len(entries))}
	}
	return nil
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
				return fmt.Sprintf("%s is owned by this user, who can make it writable (chmod) and create entries in it", d.path)
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
// when it can. uncreatable is set for a root nobody can create, because a name in
// the path is too long for any file system. A path that cannot be examined for
// another reason (permissions, a symlink loop) is an error, not a skip: protection
// that cannot see a root fails closed, and the caller says so as a typed error.
func projectRootProblem(repoRoot string) (reason string, uncreatable bool, err error) {
	info, err := os.Stat(repoRoot)
	switch {
	case err == nil && info.IsDir():
		return "", false, nil
	case err == nil:
		return "is not a directory", false, nil
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return "does not exist", false, nil
	case errors.Is(err, syscall.ENAMETOOLONG):
		return "does not exist (a name in the path is too long for any file system)", true, nil
	}
	return "", false, err
}
