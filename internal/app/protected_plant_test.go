package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// fakeCodexChmodPlanter is the next thing an agent can try: it owns the directory
// above a missing project root, so it makes that directory writable and plants a
// layer in the root it then creates.
const fakeCodexChmodPlanter = `#!/bin/sh
chmod u+w %q 2>/dev/null
if mkdir -p %q 2>/dev/null && echo planted > %q/x.yaml 2>/dev/null; then r=planted; else r=denied; fi
echo "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"dead=$r\"}}"
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`

// "Not writable" does not put a directory out of an agent's reach: the agent is the
// user who owns it, and can chmod it. A missing project root under a read-only
// directory this user owns used to be skipped on the strength of access(2), and the
// agent planted a layer anyway (the delta review of CW-20261003-0092). Protection
// cannot create the root there without changing the user's directory, so it fails
// closed: the launch is refused, naming the project and the way past, and the agent
// never runs. This runs the launch for real, under bubblewrap, with the agent that
// would have planted.
func TestProtectedAgentCannotPlantUnderAReadOnlyDirectoryItOwns(t *testing.T) {
	svc, catalog, _ := tetherLayout(t)
	clearWritableRoots(t)
	if err := ProbeBwrap(catalog); err != nil {
		t.Skipf("bubblewrap cannot build a protecting sandbox on this host: %v", err)
	}
	base := t.TempDir()
	live := filepath.Join(base, "live-repo")
	ro := filepath.Join(base, "read-only")
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
	sessID, ws, err := launchFakeCodex(t, svc, fmt.Sprintf(fakeCodexChmodPlanter, ro, deadAgents, deadAgents), nil, "--sandbox", "danger-full-access")
	if err == nil {
		// Protection let the launch through: show what the agent then did.
		_ = svc.SendTurn(context.Background(), sessID, "plant a layer")
		t.Fatalf("the launch was allowed although an agent can get past %s; the agent then reported %q", ro, waitForLog(t, ws.LogPath, "dead="))
	}
	var layerErr *config.UnprotectableLayerError
	if !errors.Is(err, launch.ErrProtectionUnavailable) || !errors.As(err, &layerErr) || layerErr.Project != "dead" {
		t.Fatalf("err = %v; want launch.ErrProtectionUnavailable wrapping an UnprotectableLayerError for dead", err)
	}
	for _, want := range []string{`"dead"`, ro, "owned by this user"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message %q does not mention %s", err.Error(), want)
		}
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatalf("the refused launch created %s (%v)", dead, err)
	}
	if fi, err := os.Stat(ro); err != nil || fi.Mode().Perm() != 0o500 {
		t.Fatalf("the user's directory %s was changed: %v %v", ro, fi, err)
	}
}
