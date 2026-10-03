package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
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
