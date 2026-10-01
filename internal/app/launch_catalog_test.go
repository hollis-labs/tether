package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

// newLaunchCatalogService loads a one-launch catalog the way New does and
// returns a Service over it plus a writer for files under its root.
func newLaunchCatalogService(t *testing.T) (*Service, func(rel, body string)) {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // keep the user discovery layer empty
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	write("global.yaml", "version: 0.1.0\n")
	write("projects/p.yaml", "id: p\nname: P\nrepo_root: "+t.TempDir()+"\n")
	write("agents/a.yaml", "id: a\nname: A\n")
	write("providers/cli.yaml", "id: cli\ntype: cli\ncommand: echo\n")
	write("launches/first.yaml", "id: first\nproject: p\nagent: a\nprovider: cli\n")

	cat, err := config.LoadLayered(root)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	if err := cat.Validate(); err != nil {
		t.Fatalf("validate catalog: %v", err)
	}
	return &Service{Catalog: cat, CatalogRoot: root}, write
}

// A launch file added or edited after startup is used by the next launch,
// with no restart (CW-20261001-0018).
func TestResolve_ReadsLaunchesFreshFromDisk(t *testing.T) {
	svc, write := newLaunchCatalogService(t)

	write("launches/second.yaml", "id: second\nproject: p\nagent: a\nprovider: cli\n")
	plan, err := svc.Resolve("second")
	if err != nil {
		t.Fatalf("resolve launch added after startup: %v", err)
	}
	if plan.LaunchID != "second" || plan.LogicalAgentID != "a" {
		t.Fatalf("plan = launch %q agent %q, want second/a", plan.LaunchID, plan.LogicalAgentID)
	}

	write("launches/first.yaml", "id: first\nproject: p\nagent: a\nprovider: cli\noverrides:\n  env:\n    EDITED: \"yes\"\n")
	plan, err = svc.resolveWithInput(CreateSessionInput{LaunchID: "first"})
	if err != nil {
		t.Fatalf("resolve edited launch: %v", err)
	}
	if plan.Env["EDITED"] != "yes" {
		t.Fatalf("edited launch override not picked up: env = %v", plan.Env)
	}

	if _, ok := svc.Catalog.Launches["second"]; ok {
		t.Fatal("the fresh read mutated the startup catalog")
	}
}

func TestResolve_UnknownLaunch_NotFoundListsDiskLaunches(t *testing.T) {
	svc, write := newLaunchCatalogService(t)
	write("launches/second.yaml", "id: second\nproject: p\nagent: a\nprovider: cli\n")

	_, err := svc.Resolve("nope")
	if !errors.Is(err, launch.ErrLaunchNotFound) {
		t.Fatalf("err = %v, want ErrLaunchNotFound", err)
	}
	if !strings.Contains(err.Error(), "known launches: first, second") {
		t.Fatalf("message should list the launches on disk: %v", err)
	}
}

// Only launches are re-read: a new launch naming an agent the daemon has
// not loaded is refused with an error that says why, rather than resolved
// against an agent session creation cannot see.
func TestResolve_LaunchNamingNewAgent_RefusedWithRestartHint(t *testing.T) {
	svc, write := newLaunchCatalogService(t)
	write("agents/b.yaml", "id: b\nname: B\n")
	write("launches/uses-b.yaml", "id: uses-b\nproject: p\nagent: b\nprovider: cli\n")

	_, err := svc.Resolve("uses-b")
	if err == nil {
		t.Fatal("want an error for a launch naming an agent loaded after startup")
	}
	if errors.Is(err, launch.ErrLaunchNotFound) {
		t.Fatalf("this is not a missing launch: %v", err)
	}
	if !strings.Contains(err.Error(), `unknown agent "b"`) || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("message = %v, want the unknown agent and a restart hint", err)
	}
}

// A catalog that stops loading does not stop launches it already knew.
func TestResolve_BrokenCatalog_FallsBackToStartupLaunches(t *testing.T) {
	svc, write := newLaunchCatalogService(t)
	write("launches/broken.yaml", "id: [unclosed\n")

	if _, err := svc.Resolve("first"); err != nil {
		t.Fatalf("startup launch should still resolve: %v", err)
	}
	_, err := svc.Resolve("nope")
	if !errors.Is(err, launch.ErrLaunchNotFound) {
		t.Fatalf("err = %v, want ErrLaunchNotFound", err)
	}
	if !strings.Contains(err.Error(), "re-reading the catalog failed") {
		t.Fatalf("message should carry the reload failure: %v", err)
	}
}

func TestResolve_InvalidMCPGrantDoesNotFallBack(t *testing.T) {
	svc, write := newLaunchCatalogService(t)
	write("launches/first.yaml", "id: first\nproject: p\nagent: a\nprovider: cli\nmcp:\n  servers: [missing]\n")
	if _, err := svc.Resolve("first"); !errors.Is(err, config.ErrInvalidMCPGrant) {
		t.Fatalf("must refuse an invalid reloaded grant, not fall back: %v", err)
	}
}
