package app

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hollis-labs/agentkit/agentsessions"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

// deadRootService is a protected Service whose catalog registers projects the
// way the live one did when every protected launch was refused
// (CW-20261003-0092): some with a real repo_root, some whose repo_root is gone.
// Every dead root is under a writable temp directory, so an agent could create it.
func deadRootService(t *testing.T) (svc *Service, existing, dead map[string]string) {
	t.Helper()
	svc, _, _ = tetherLayoutKeepingMode(t)
	base := t.TempDir()
	svc.Catalog.Projects = map[string]config.Project{}
	existing, dead = map[string]string{}, map[string]string{}
	for _, id := range []string{"live-a", "live-b", "live-c", "live-d"} {
		root := filepath.Join(base, "repos", id)
		if err := os.MkdirAll(root, 0o750); err != nil {
			t.Fatal(err)
		}
		existing[id] = root
		svc.Catalog.Projects[id] = config.Project{RepoRoot: root}
	}
	dead["dead-a"] = filepath.Join(base, "repos", "dead-a")          // parent exists
	dead["dead-b"] = filepath.Join(base, "no-such-parent", "dead-b") // parent missing too
	dead["dead-c"] = filepath.Join(base, "repos", "dead-c")
	for id, root := range dead {
		svc.Catalog.Projects[id] = config.Project{RepoRoot: root}
	}
	return svc, existing, dead
}

func noLayerCreatedIn(t *testing.T, roots map[string]string) {
	t.Helper()
	for id, root := range roots {
		if _, err := os.Stat(filepath.Join(root, ".tether")); !os.IsNotExist(err) {
			t.Fatalf("project %s: a layer was created in %s although the call was refused (%v)", id, root, err)
		}
	}
}

func noneExist(t *testing.T, roots map[string]string) {
	t.Helper()
	for id, root := range roots {
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("project %s: %s exists although the call was refused (%v)", id, root, err)
		}
	}
}

// One project with a repo_root that is gone must not refuse the launches of
// every other project. This is the live failure: POST /sessions answered 500
// 'protect catalog layer: parent unavailable' for any protected launch. And it
// must not be fixed by leaving the dead project's layer open: an agent could
// create that root and plant a layer, so each missing root an agent could create
// is created holding only .tether, and that layer is protected like the others.
func TestAProtectedLaunchSurvivesProjectsWithAMissingRepoRoot(t *testing.T) {
	svc, existing, dead := deadRootService(t)
	opts := agentsessions.StartOptions{Workdir: t.TempDir(), WorkspaceDir: t.TempDir()}
	plan := &launch.Plan{ProviderBrand: "claude", ProjectID: "live-a"}
	if err := svc.applyControlPlaneProtection(plan, "cli", &opts); err != nil {
		t.Fatalf("a dead project refused another project's launch: %v", err)
	}
	for _, roots := range []map[string]string{existing, dead} {
		for id, root := range roots {
			layer, err := filepath.EvalSymlinks(filepath.Join(root, ".tether"))
			if err != nil || !containsPath(opts.ProtectedPaths, layer) {
				t.Fatalf("project %s: its layer is not protected: %q (%v)", id, opts.ProtectedPaths, err)
			}
		}
	}
	for id, root := range dead {
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 1 || entries[0].Name() != ".tether" {
			t.Fatalf("project %s: the created root = %v (%v); want only .tether", id, entries, err)
		}
	}
}

// The project a launch is for is different: if its own repo_root is gone the
// launch is refused with a typed error naming the project and path, not a bare
// internal error, and the refusal creates nothing: not its root, not a layer in
// any live project, not the other dead projects' roots.
func TestALaunchForAProjectWithAMissingRepoRootIsATypedRefusal(t *testing.T) {
	svc, existing, dead := deadRootService(t)
	opts := agentsessions.StartOptions{Workdir: t.TempDir(), WorkspaceDir: t.TempDir()}
	plan := &launch.Plan{ProviderBrand: "claude", ProjectID: "dead-a"}
	err := svc.applyControlPlaneProtection(plan, "cli", &opts)
	if !errors.Is(err, launch.ErrLaunchProjectRootMissing) {
		t.Fatalf("err = %v; want launch.ErrLaunchProjectRootMissing", err)
	}
	var rootErr *config.ProjectRootError
	if !errors.As(err, &rootErr) || rootErr.Project != "dead-a" || rootErr.Root != dead["dead-a"] {
		t.Fatalf("err = %v; want a *config.ProjectRootError naming dead-a and %s", err, dead["dead-a"])
	}
	for _, want := range []string{`"dead-a"`, dead["dead-a"], "does not exist"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message %q does not mention %s", err.Error(), want)
		}
	}
	if opts.ProtectedPaths != nil {
		t.Fatalf("a refused launch was given protected paths: %q", opts.ProtectedPaths)
	}
	noLayerCreatedIn(t, existing)
	noneExist(t, dead)

	// Session create goes through refuseUnprotectable: that is the call the live
	// daemon answered 500 for.
	if err := svc.refuseUnprotectable(plan, "cli"); !errors.Is(err, launch.ErrLaunchProjectRootMissing) {
		t.Fatalf("create for a dead project: err = %v; want launch.ErrLaunchProjectRootMissing", err)
	}
	// The same refusal for the other kind of dead root (its parent is missing too).
	if err := svc.applyControlPlaneProtection(&launch.Plan{ProviderBrand: "claude", ProjectID: "dead-b"}, "cli", &opts); !errors.Is(err, launch.ErrLaunchProjectRootMissing) {
		t.Fatalf("dead-b: err = %v", err)
	}
	// An MCP proxy launch for that project is refused the same way.
	if _, err := svc.mcpProtectedPaths(plan); !errors.As(err, &rootErr) {
		t.Fatalf("mcpProtectedPaths for a dead project: %v", err)
	}
	noLayerCreatedIn(t, existing)
	noneExist(t, dead)

	// A launch for a live project is unaffected by the dead ones (and is the call
	// that anchors them).
	if err := svc.refuseUnprotectable(&launch.Plan{ProviderBrand: "claude", ProjectID: "live-a"}, "cli"); err != nil {
		t.Fatalf("create for a live project was refused: %v", err)
	}

	// That call created every dead root, so the dead projects' own roots now exist,
	// as placeholders. A launch for one of them is still the typed refusal (a daemon
	// anchors every root when it starts, so otherwise the refusal could never fire),
	// and it runs nothing in the placeholder.
	for _, id := range []string{"dead-a", "dead-b"} {
		for name, call := range map[string]func() error{
			"create": func() error {
				return svc.refuseUnprotectable(&launch.Plan{ProviderBrand: "claude", ProjectID: id}, "cli")
			},
			"launch": func() error {
				return svc.applyControlPlaneProtection(&launch.Plan{ProviderBrand: "claude", ProjectID: id}, "cli", &agentsessions.StartOptions{Workdir: t.TempDir(), WorkspaceDir: t.TempDir()})
			},
		} {
			err := call()
			if !errors.Is(err, launch.ErrLaunchProjectRootMissing) || !errors.As(err, &rootErr) || rootErr.Project != id || !strings.Contains(err.Error(), "placeholder") {
				t.Fatalf("%s for %s after its root became a placeholder: err = %v; want the typed refusal naming the placeholder", name, id, err)
			}
		}
	}
}

// A file where a project's root should be, in a directory an agent can write to,
// is something an agent could replace with a directory and plant a layer into,
// and Tether will not delete a user's file: the launch is refused, naming the
// project, with its own sentinel (and API code): this is a catalog problem, not a
// host that cannot provide protection (no bubblewrap), which reads the same way.
func TestAFileInTheWayOfAProjectRootRefusesTheLaunch(t *testing.T) {
	svc, existing, _ := deadRootService(t)
	file := filepath.Join(t.TempDir(), "not-a-repo")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc.Catalog.Projects["isfile"] = config.Project{RepoRoot: file}
	_, _, err := svc.protectionPlan(&launch.Plan{ProviderBrand: "claude", ProjectID: "live-a"}, "cli", nil, false)
	var layerErr *config.UnprotectableLayerError
	if !errors.Is(err, launch.ErrProjectLayerUnprotectable) || errors.Is(err, launch.ErrProtectionUnavailable) || !errors.As(err, &layerErr) || layerErr.Project != "isfile" {
		t.Fatalf("err = %v; want launch.ErrProjectLayerUnprotectable (and not ErrProtectionUnavailable) wrapping an UnprotectableLayerError for isfile", err)
	}
	if !strings.Contains(err.Error(), `"isfile"`) || !strings.Contains(err.Error(), "a file is in the way") {
		t.Fatalf("message %q must name the project and the cause", err.Error())
	}
	noLayerCreatedIn(t, existing)
}

// The created roots are visible in /health on every call, not only the one that
// created them: the root exists afterwards, but it is still only the placeholder,
// and that is what the report says. Nothing is reported as skipped, because
// nothing was left open.
func TestProtectionHealthReportsCreatedProjectRoots(t *testing.T) {
	svc, _, dead := deadRootService(t)
	for i := 0; i < 3; i++ { // each call after the first sees the placeholders it left
		h := svc.ProtectionHealth()
		if h.PlanError != "" || len(h.SkippedProjectLayers) != 0 {
			t.Fatalf("call %d: plan error %q, skipped %+v; want neither", i, h.PlanError, h.SkippedProjectLayers)
		}
		if len(h.CreatedProjectRoots) != len(dead) {
			t.Fatalf("call %d: created roots = %+v; want the %d dead projects", i, h.CreatedProjectRoots, len(dead))
		}
		for _, c := range h.CreatedProjectRoots {
			if dead[c.Project] != c.RepoRoot || !strings.Contains(c.Reason, "read-only") {
				t.Fatalf("call %d: created entry %+v does not match the catalog (%v)", i, c, dead)
			}
		}
		if runtime.GOOS == "linux" && (!h.BwrapChecked || !h.BwrapUsable || h.BwrapError != "") {
			t.Fatalf("call %d: the bubblewrap probe must report on its own: %+v", i, h)
		}
	}
}

// Only a root no agent could create is skipped, and that is what /health lists as
// skipped. The agent is this user, so that takes a directory nothing in its reach
// controls: a system directory (every directory a test makes is the user's own).
func TestProtectionHealthReportsSkippedProjectLayersOnlyWhereNoAgentCouldCreateTheRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("write permission cannot be taken away from root")
	}
	svc, _, _ := deadRootService(t)
	for _, sys := range []string{"/usr/share", "/usr/lib", "/usr/include", "/opt", "/etc"} {
		root := filepath.Join(sys, "tether-protection-test-no-such-dir", "repo")
		svc.Catalog.Projects["ro"] = config.Project{RepoRoot: root}
		h := svc.ProtectionHealth()
		if h.PlanError != "" { // this host lets the user get past it: try the next
			continue
		}
		if len(h.SkippedProjectLayers) != 1 || h.SkippedProjectLayers[0].Project != "ro" ||
			!strings.Contains(h.SkippedProjectLayers[0].Reason, "an agent cannot create it either") {
			t.Fatalf("skipped = %+v; want only the project under %s", h.SkippedProjectLayers, sys)
		}
		if _, err := os.Stat(filepath.Dir(root)); !os.IsNotExist(err) {
			t.Fatalf("a root no agent could create was created (%v)", err)
		}
		return
	}
	t.Skip("no system directory that this user can neither write nor get past")
}

// A directory that is not writable but that this user owns is not a reason to skip
// (the agent can chmod it), and not a reason to refuse every launch either (one dead
// project under an unmounted mountpoint used to refuse every protected launch, 500
// every Codex launch and break the MCP gateway): it is anchored read-only. /health
// says so, no launch is refused, and the cost is the one every anchor has: a launch
// whose own directories lie inside it is refused.
func TestAReadOnlyDirectoryTheUserOwnsIsAnchoredNotSkippedOrRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("write permission cannot be taken away from root")
	}
	svc, existing, _ := deadRootService(t)
	ro := filepath.Join(t.TempDir(), "read-only")
	if err := os.MkdirAll(ro, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o750) })
	realRO, err := filepath.EvalSymlinks(ro)
	if err != nil {
		t.Fatal(err)
	}
	svc.Catalog.Projects = map[string]config.Project{"live-a": {RepoRoot: existing["live-a"]}, "ro": {RepoRoot: filepath.Join(ro, "repo")}}

	h := svc.ProtectionHealth()
	if h.PlanError != "" || len(h.SkippedProjectLayers) != 0 {
		t.Fatalf("plan error = %q, skipped = %+v; want neither: nothing is refused and nothing is left open", h.PlanError, h.SkippedProjectLayers)
	}
	if len(h.AnchoredProjectAncestors) != 1 || h.AnchoredProjectAncestors[0].Project != "ro" || h.AnchoredProjectAncestors[0].Ancestor != realRO {
		t.Fatalf("anchored ancestors = %+v; want ro anchored at %s", h.AnchoredProjectAncestors, realRO)
	}
	dirs, _, err := svc.protectionPlan(&launch.Plan{ProviderBrand: "claude", ProjectID: "live-a"}, "cli", nil, false)
	if err != nil || !containsPath(dirs, realRO) {
		t.Fatalf("protection plan: dirs = %q, err = %v; want no refusal and %s anchored", dirs, err, realRO)
	}
	// The cost: a launch that would work inside the anchored directory is refused,
	// like one inside any other anchor, with the reason.
	opts := agentsessions.StartOptions{Workdir: filepath.Join(ro, "work"), WorkspaceDir: t.TempDir()}
	if err := svc.applyControlPlaneProtection(&launch.Plan{ProviderBrand: "claude", ProjectID: "live-a"}, "cli", &opts); !errors.Is(err, launch.ErrLaunchInsideProtectedPath) {
		t.Fatalf("a launch working inside the anchored directory: err = %v; want launch.ErrLaunchInsideProtectedPath", err)
	}
	if _, err := os.Stat(filepath.Join(ro, "repo")); !os.IsNotExist(err) {
		t.Fatalf("something was created under the anchored directory (%v)", err)
	}
}

// A plan failure that is not a missing root is reported as what it is. It used to
// leave the probe directory empty and report bubblewrap as failing with 'the
// catalog root is not set', which sent operators after the wrong thing (the live
// /health said exactly that while bubblewrap worked).
func TestProtectionHealthNamesAPlanFailureInsteadOfBlamingBubblewrap(t *testing.T) {
	svc, _, _ := deadRootService(t)
	svc.Catalog.Projects["rootfs"] = config.Project{RepoRoot: "/"}
	h := svc.ProtectionHealth()
	if !strings.Contains(h.PlanError, `project "rootfs"`) {
		t.Fatalf("plan error = %q; want it to name the project", h.PlanError)
	}
	if strings.Contains(h.BwrapError, "catalog root is not set") {
		t.Fatalf("the plan failure was blamed on the catalog root being unset: %+v", h)
	}
	if runtime.GOOS == "linux" && (!h.BwrapChecked || !h.BwrapUsable || h.BwrapError != "") {
		t.Fatalf("the bubblewrap probe must still run and report on its own: %+v", h)
	}
}

// What protection does to a missing root is logged: a root it created is warned
// about once per daemon, and the layer creation is logged, so neither is silent
// and neither repeats on every launch.
func TestCreatedProjectRootsAreWarnedAboutOncePerDaemon(t *testing.T) {
	svc, existing, dead := deadRootService(t)
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	for i := 0; i < 3; i++ {
		if _, err := svc.controlPlaneDirs(); err != nil {
			t.Fatal(err)
		}
	}
	out := buf.String()
	for id, root := range dead {
		if n := strings.Count(out, `WARN: protect: project "`+id+`": its repo_root `+root+` does not exist and an agent could have created it`); n != 1 {
			t.Fatalf("project %s was warned about %d times in 3 launches; want exactly once:\n%s", id, n, out)
		}
		if n := strings.Count(out, "created the empty layer directory "+filepath.Join(root, ".tether")); n != 1 {
			t.Fatalf("project %s: its layer creation was logged %d times in 3 launches; want once:\n%s", id, n, out)
		}
	}
	for id, root := range existing {
		if n := strings.Count(out, "created the empty layer directory "+filepath.Join(root, ".tether")); n != 1 {
			t.Fatalf("project %s: its layer creation was logged %d times in 3 launches; want once:\n%s", id, n, out)
		}
	}
}
