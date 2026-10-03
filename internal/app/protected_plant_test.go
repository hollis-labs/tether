package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

// fakeCodexPlanter stands in for an agent that tries to plant a project layer:
// it creates <root>/.tether/agents/x.yaml for a project whose root is missing
// (the reviewer's exploit, CW-20261003-0092) and for a project whose root exists,
// and reports which attempts succeeded.
const fakeCodexPlanter = `#!/bin/sh
try() { if mkdir -p "$1" 2>/dev/null && echo planted > "$1/x.yaml" 2>/dev/null; then echo planted; else echo denied; fi; }
msg="dead=$(try %q) live=$(try %q)"
echo "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"$msg\"}}"
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`

// The sandbox is the whole host, writable, with only Tether's own directories
// bound read-only. A project whose repo_root is missing, under a writable parent,
// is therefore somewhere an agent can create <root>/.tether/agents/x.yaml, and the
// next catalog load reads that layer for the project. Protection must close it, not
// skip it: the root is created holding only .tether, which is bound read-only. This
// runs the attempt for real, through LaunchSession under bubblewrap.
func TestProtectedAgentCannotPlantALayerInAMissingProjectRoot(t *testing.T) {
	svc, catalog, _ := tetherLayout(t)
	clearWritableRoots(t)
	if err := ProbeBwrap(catalog); err != nil {
		t.Skipf("bubblewrap cannot build a protecting sandbox on this host: %v", err)
	}
	base := t.TempDir()
	live, dead := filepath.Join(base, "live-repo"), filepath.Join(base, "gone-repo")
	if err := os.MkdirAll(live, 0o750); err != nil {
		t.Fatal(err)
	}
	// "proj" is the project the stand-in's launch is for; "dead" is another one.
	svc.Catalog.Projects = map[string]config.Project{"proj": {RepoRoot: live}, "dead": {RepoRoot: dead}}

	deadAgents, liveAgents := filepath.Join(dead, ".tether", "agents"), filepath.Join(live, ".tether", "agents")
	sessID, ws := startFakeCodex(t, svc, fmt.Sprintf(fakeCodexPlanter, deadAgents, liveAgents), "--sandbox", "danger-full-access")
	if err := svc.SendTurn(context.Background(), sessID, "plant a layer"); err != nil {
		t.Fatalf("turn: %v", err)
	}
	logData := waitForLog(t, ws.LogPath, "dead=")
	if !strings.Contains(logData, "dead=denied live=denied") {
		t.Fatalf("agent plants = %q; want both denied: an agent planted a project layer", logData)
	}
	for _, planted := range []string{filepath.Join(deadAgents, "x.yaml"), filepath.Join(liveAgents, "x.yaml")} {
		if _, err := os.Stat(planted); !os.IsNotExist(err) {
			t.Fatalf("the agent planted %s (stat err = %v)", planted, err)
		}
	}
	// What protection left for the dead project is the root with only .tether, which
	// holds only the placeholder marker.
	entries, err := os.ReadDir(dead)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".tether" {
		t.Fatalf("the created root = %v (%v); want only .tether", entries, err)
	}
	if layer, err := os.ReadDir(filepath.Join(dead, ".tether")); err != nil || len(layer) != 1 || layer[0].Name() != config.PlaceholderMarker {
		t.Fatalf("the anchored layer holds %v (%v); want only %s", layer, err, config.PlaceholderMarker)
	}
}

// fakeCodexChmodPlanter is the next thing an agent can try. It owns the directory
// above a missing project root (and the directory above that), so it makes the
// directory writable, plants a layer in the root it then creates, and, failing
// that, tries to move the directory, and the one above it, out of the way so that
// it can put its own in their place. It reports each attempt.
const fakeCodexChmodPlanter = `#!/bin/sh
chmod u+w %[1]q 2>/dev/null && chmod=ok || chmod=denied
if mkdir -p %[3]q 2>/dev/null && echo planted > %[3]q/x.yaml 2>/dev/null; then plant=planted; else plant=denied; fi
if mv %[1]q %[1]q-moved 2>/dev/null; then mvA=moved; else mvA=denied; fi
if mv %[2]q %[2]q-moved 2>/dev/null; then mvP=moved; else mvP=denied; fi
echo "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"chmod=$chmod plant=$plant mvA=$mvA mvP=$mvP\"}}"
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`

// "Not writable" does not put a directory out of an agent's reach: the agent is the
// user who owns it, and can chmod it, or move it (and every directory above it)
// aside. A missing project root under a read-only directory this user owns used to
// be skipped on the strength of access(2), and the agent planted a layer anyway
// (the delta review of CW-20261003-0092); refusing every launch instead made one
// dead project under an unmounted mountpoint take out every protected launch. So the
// directory is anchored read-only, which also pins it and every directory above it
// (each is a mount point): the launch is allowed, and the agent can do none of
// those things. This runs it for real, through LaunchSession under bubblewrap, with
// the agent that would have planted.
func TestProtectedAgentCannotPlantUnderAReadOnlyDirectoryItOwns(t *testing.T) {
	svc, catalog, _ := tetherLayout(t)
	clearWritableRoots(t)
	if err := ProbeBwrap(catalog); err != nil {
		t.Skipf("bubblewrap cannot build a protecting sandbox on this host: %v", err)
	}
	base := t.TempDir()
	live := filepath.Join(base, "live-repo")
	holder := filepath.Join(base, "holder")
	ro := filepath.Join(holder, "read-only")
	for _, d := range []string{live, ro} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o750) })
	dead := filepath.Join(ro, "gone-repo")
	svc.Catalog.Projects = map[string]config.Project{"proj": {RepoRoot: live}, "dead": {RepoRoot: dead}}

	deadAgents := filepath.Join(dead, ".tether", "agents")
	sessID, ws, err := launchFakeCodex(t, svc, fmt.Sprintf(fakeCodexChmodPlanter, ro, holder, deadAgents), nil, "--sandbox", "danger-full-access")
	if err != nil {
		t.Fatalf("a dead project under a read-only directory refused the launch of another project: %v", err)
	}
	if err := svc.SendTurn(context.Background(), sessID, "plant a layer"); err != nil {
		t.Fatalf("turn: %v", err)
	}
	logData := waitForLog(t, ws.LogPath, "plant=")
	t.Logf("the agent reported: %s", strings.TrimSpace(logData))
	if !strings.Contains(logData, "plant=denied mvA=denied mvP=denied") {
		t.Fatalf("agent attempts = %q; want the plant and both renames denied", logData)
	}
	if _, err := os.Stat(filepath.Join(deadAgents, "x.yaml")); !os.IsNotExist(err) {
		t.Fatalf("the agent planted a layer under the read-only directory (stat err = %v)", err)
	}
	for _, d := range []string{ro, holder} {
		if _, err := os.Stat(d + "-moved"); !os.IsNotExist(err) {
			t.Fatalf("the agent moved %s aside (stat err = %v)", d, err)
		}
	}
}

// fakeCodexRootDropper stands in for an agent that drops one file into the root of a
// project whose root protection made a placeholder (only .tether is anchored, the
// rest of the root is the agent's to write).
const fakeCodexRootDropper = `#!/bin/sh
if echo dropped > %[1]q/dropped.txt 2>/dev/null; then r=dropped; else r=denied; fi
if echo planted > %[1]q/.tether/x.yaml 2>/dev/null; then p=planted; else p=denied; fi
echo "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"file=$r layer=$p\"}}"
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`

// A placeholder was told by "the root holds only .tether", so one file dropped into
// the root by an agent turned it back into a project that launches: the typed 409
// was defeated, the created-roots report emptied, and the project vanished from the
// listing after a restart. It is a placeholder while it holds Tether's marker,
// whatever else is in the root. This does it for real, under bubblewrap: the agent
// can drop the file (and cannot touch the layer), and the dead project's own launch
// is still refused, and still reported.
func TestProtectedAgentCannotDefeatThePlaceholderByDroppingAFileInTheRoot(t *testing.T) {
	svc, catalog, _ := tetherLayout(t)
	clearWritableRoots(t)
	if err := ProbeBwrap(catalog); err != nil {
		t.Skipf("bubblewrap cannot build a protecting sandbox on this host: %v", err)
	}
	base := t.TempDir()
	live, dead := filepath.Join(base, "live-repo"), filepath.Join(base, "gone-repo")
	if err := os.MkdirAll(live, 0o750); err != nil {
		t.Fatal(err)
	}
	svc.Catalog.Projects = map[string]config.Project{"proj": {RepoRoot: live}, "dead": {RepoRoot: dead}}

	sessID, ws, err := launchFakeCodex(t, svc, fmt.Sprintf(fakeCodexRootDropper, dead), nil, "--sandbox", "danger-full-access")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if err := svc.SendTurn(context.Background(), sessID, "drop a file"); err != nil {
		t.Fatalf("turn: %v", err)
	}
	logData := waitForLog(t, ws.LogPath, "layer=")
	if !strings.Contains(logData, "file=dropped layer=denied") {
		t.Fatalf("agent = %q; want it to drop a file in the root and fail to touch the layer", logData)
	}
	if _, err := os.Stat(filepath.Join(dead, "dropped.txt")); err != nil {
		t.Fatalf("the agent's file is not there (%v): the experiment did not run", err)
	}
	plan := &launch.Plan{ProviderBrand: "claude", ProjectID: "dead"}
	var rootErr *config.ProjectRootError
	if err := svc.refuseUnprotectable(plan, "cli"); !errors.Is(err, launch.ErrLaunchProjectRootMissing) || !errors.As(err, &rootErr) || !strings.Contains(err.Error(), config.PlaceholderMarker) {
		t.Fatalf("a launch for the dead project after the agent dropped a file in its root: err = %v; want the typed refusal naming the marker", err)
	}
	h := svc.ProtectionHealth()
	if len(h.CreatedProjectRoots) != 1 || h.CreatedProjectRoots[0].Project != "dead" || !strings.Contains(h.CreatedProjectRoots[0].Reason, "something else has been put in the directory since") {
		t.Fatalf("created project roots = %+v; want dead, still reported, saying something was put in it", h.CreatedProjectRoots)
	}
}

// fakeCodexLinkReplacer stands in for the agent of the symlink bypass: it unlinks
// the link a project's repo_root is, and puts a real directory with a planted layer
// where the link was.
const fakeCodexLinkReplacer = `#!/bin/sh
rm -f %[1]q 2>/dev/null
if mkdir -p %[1]q/.tether/agents 2>/dev/null && echo planted > %[1]q/.tether/agents/x.yaml 2>/dev/null; then r=planted; else r=denied; fi
echo "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"link=$r\"}}"
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`

// systemTreeNoAgentCanChange finds a directory under which nothing can be created
// by a process running as this user, nor moved aside: every directory from it up to
// / is owned by someone else and not writable by this user (see config's tests).
func systemTreeNoAgentCanChange(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("write permission cannot be taken away from root")
	}
	for _, dir := range []string{"/usr/share", "/usr/lib", "/usr/include", "/opt"} {
		reach := false
		for cur := dir; ; cur = filepath.Dir(cur) {
			fi, err := os.Stat(cur)
			if err != nil {
				reach = true
				break
			}
			if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) == os.Geteuid() || syscall.Access(cur, 0x2) == nil {
				reach = true
				break
			}
			if filepath.Dir(cur) == cur {
				break
			}
		}
		if !reach {
			return dir
		}
	}
	t.Skip("no system directory that this user can neither write nor get past")
	return ""
}

// A symlink repo_root cannot be protected where an agent can replace it: it unlinks
// the link and puts a real directory with a planted layer there, and the loader
// reads the layer through the link. A dangling link whose target chain is root-owned
// was SKIPPED (nothing could be created at the target), and the agent planted
// anyway, on both c207ca1 and b17d177 (the review of PR #149). The launch is refused
// instead, naming the project and the link, and the agent never runs.
func TestProtectedAgentCannotReplaceASymlinkRoot(t *testing.T) {
	svc, catalog, _ := tetherLayout(t)
	clearWritableRoots(t)
	if err := ProbeBwrap(catalog); err != nil {
		t.Skipf("bubblewrap cannot build a protecting sandbox on this host: %v", err)
	}
	sys := systemTreeNoAgentCanChange(t)
	base := t.TempDir()
	live := filepath.Join(base, "live-repo")
	if err := os.MkdirAll(live, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "linked-repo")
	if err := os.Symlink(filepath.Join(sys, "tether-protection-test-no-such-dir", "repo"), link); err != nil {
		t.Fatal(err)
	}
	svc.Catalog.Projects = map[string]config.Project{"proj": {RepoRoot: live}, "linked": {RepoRoot: link}}

	sessID, ws, err := launchFakeCodex(t, svc, fmt.Sprintf(fakeCodexLinkReplacer, link), nil, "--sandbox", "danger-full-access")
	if err == nil {
		_ = svc.SendTurn(context.Background(), sessID, "replace the link")
		t.Fatalf("the launch was allowed although an agent can replace %s; the agent then reported %q", link, waitForLog(t, ws.LogPath, "link="))
	}
	var layerErr *config.UnprotectableLayerError
	if !errors.Is(err, launch.ErrProjectLayerUnprotectable) || !errors.As(err, &layerErr) || layerErr.Project != "linked" {
		t.Fatalf("err = %v; want launch.ErrProjectLayerUnprotectable wrapping an UnprotectableLayerError for linked", err)
	}
	for _, want := range []string{`"linked"`, link, "symlink an agent can replace"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message %q does not mention %s", err.Error(), want)
		}
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced or removed by a refused launch (%v)", err)
	}
}
