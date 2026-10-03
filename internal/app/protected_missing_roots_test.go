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

// One project with a repo_root that is gone must not refuse the launches of
// every other project. This is the live failure: POST /sessions answered 500
// 'protect catalog layer: parent unavailable' for any protected launch.
func TestAProtectedLaunchSurvivesProjectsWithAMissingRepoRoot(t *testing.T) {
	svc, existing, dead := deadRootService(t)
	opts := agentsessions.StartOptions{Workdir: t.TempDir(), WorkspaceDir: t.TempDir()}
	plan := &launch.Plan{ProviderBrand: "claude", ProjectID: "live-a"}
	if err := svc.applyControlPlaneProtection(plan, "cli", &opts); err != nil {
		t.Fatalf("a dead project refused another project's launch: %v", err)
	}
	for id, root := range existing {
		layer, err := filepath.EvalSymlinks(filepath.Join(root, ".tether"))
		if err != nil || !containsPath(opts.ProtectedPaths, layer) {
			t.Fatalf("project %s: its layer is not protected: %q (%v)", id, opts.ProtectedPaths, err)
		}
	}
	for id, root := range dead {
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("project %s: a missing repo_root %s was created (%v)", id, root, err)
		}
		for _, p := range opts.ProtectedPaths {
			if strings.Contains(p, id) {
				t.Fatalf("project %s: a dead project's path is among the protected paths: %q", id, opts.ProtectedPaths)
			}
		}
	}
}

// The project a launch is for is different: if its own repo_root is gone the
// launch is refused with a typed error naming the project and path, not a bare
// internal error, and the refusal creates nothing.
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

	// The same refusal for the other kind of dead root (its parent is missing too).
	if err := svc.applyControlPlaneProtection(&launch.Plan{ProviderBrand: "claude", ProjectID: "dead-b"}, "cli", &opts); !errors.Is(err, launch.ErrLaunchProjectRootMissing) {
		t.Fatalf("dead-b: err = %v", err)
	}

	// Session create goes through refuseUnprotectable: that is the call the live
	// daemon answered 500 for. A dead project refuses only its own launches.
	if err := svc.refuseUnprotectable(plan, "cli"); !errors.Is(err, launch.ErrLaunchProjectRootMissing) {
		t.Fatalf("create for a dead project: err = %v; want launch.ErrLaunchProjectRootMissing", err)
	}
	if err := svc.refuseUnprotectable(&launch.Plan{ProviderBrand: "claude", ProjectID: "live-a"}, "cli"); err != nil {
		t.Fatalf("create for a live project was refused: %v", err)
	}

	// An MCP proxy launch for that project is refused the same way.
	if _, err := svc.mcpProtectedPaths(plan); !errors.As(err, &rootErr) {
		t.Fatalf("mcpProtectedPaths for a dead project: %v", err)
	}
}

// The skip is visible: /health reports each skipped layer, and the bubblewrap
// probe stays its own answer.
func TestProtectionHealthReportsSkippedProjectLayers(t *testing.T) {
	svc, _, dead := deadRootService(t)
	h := svc.ProtectionHealth()
	if h.PlanError != "" {
		t.Fatalf("plan error = %q; a dead project must not be one", h.PlanError)
	}
	if len(h.SkippedProjectLayers) != len(dead) {
		t.Fatalf("skipped = %+v; want %d dead projects", h.SkippedProjectLayers, len(dead))
	}
	for _, sk := range h.SkippedProjectLayers {
		if dead[sk.Project] != sk.RepoRoot || sk.Reason != "does not exist" {
			t.Fatalf("skipped entry %+v does not match the catalog (%v)", sk, dead)
		}
	}
	if runtime.GOOS == "linux" && (!h.BwrapChecked || !h.BwrapUsable || h.BwrapError != "") {
		t.Fatalf("the bubblewrap probe must report on its own: %+v", h)
	}
}

// A plan failure that is not a skippable dead project is reported as what it is.
// It used to leave the probe directory empty and report bubblewrap as failing
// with 'the catalog root is not set', which sent operators after the wrong thing
// (the live /health said exactly that while bubblewrap worked).
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

// Each skipped project is warned about once per daemon, and the layers that had
// to be created are logged, so neither the skip nor the side effect is silent
// and neither repeats on every launch.
func TestSkippedProjectsAreWarnedAboutOncePerDaemon(t *testing.T) {
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
	for id := range dead {
		if n := strings.Count(out, `project "`+id+`" is left out of control-plane protection`); n != 1 {
			t.Fatalf("project %s was warned about %d times in 3 launches; want exactly once:\n%s", id, n, out)
		}
	}
	for id := range existing {
		if n := strings.Count(out, "created the empty layer directory "+filepath.Join(existing[id], ".tether")); n != 1 {
			t.Fatalf("project %s: its layer creation was logged %d times in 3 launches; want once:\n%s", id, n, out)
		}
	}
	if !strings.Contains(out, "WARN: protect: project") {
		t.Fatalf("the skip is not a warning:\n%s", out)
	}
}
