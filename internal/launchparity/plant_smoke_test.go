package launchparity

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/agentkit/agentlaunch"
	"github.com/hollis-labs/agentkit/agentlaunch/launcher"
	"github.com/hollis-labs/agentkit/agentlaunch/parity"
	"github.com/hollis-labs/agentkit/agentlaunch/providerplant"

	"github.com/hollis-labs/tether/internal/launchresolve"
	"github.com/hollis-labs/tether/internal/specresolve"
)

// TestPlantSmoke_PermissionContract is the deterministic core of the S5
// headless-launch smoke. It drives the full spec path —
// specresolve.Resolve -> launcher.Compile -> Prepare ->
// providerplant.Plant — for a claude and a codex launch and asserts the
// approval/permission contract survives into the prepared launch's argv,
// where agentkit puts the posture's flags (CW-20261001-0156):
//
//   - claude: --permission-mode bypassPermissions (catalog default
//     permission_mode: bypass -> posture yolo).
//   - codex:  -c approval_policy="on-request" (posture accept-edits, so
//     MCP tool calls are asked for rather than refused).
//
// It does NOT execute an agent — that is the live half of the smoke. This
// half is what the orchestrator-s5 amendment is really about: a launch
// that shows parity-green must not silently drop the approval posture.
// providerplant.Plant here runs with no WithAdapter, so it goes
// through DefaultResolver — the same path the daemon session-launch uses.
//
// Skips cleanly when the live catalog or the deployed LaunchSpec corpus is
// absent.
func TestPlantSmoke_PermissionContract(t *testing.T) {
	catalogRoot := parity.DefaultCatalogRoot()
	if err := parity.RequireCatalog(catalogRoot); err != nil {
		t.Skipf("live catalog absent: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	corpus := filepath.Join(home, ".tether", "launch-specs")
	if _, err := os.Stat(filepath.Join(corpus, "launch-assembly.yaml")); err != nil {
		t.Skipf("deployed LaunchSpec corpus absent at %s: %v", corpus, err)
	}

	reg, err := launchresolve.OpenAt(launchresolve.Options{CatalogRoot: catalogRoot})
	if err != nil {
		t.Fatalf("launchresolve.OpenAt: %v", err)
	}
	res, err := specresolve.NewResolver(reg, specresolve.WithSpecsRoot(corpus))
	if err != nil {
		t.Fatalf("specresolve.NewResolver: %v", err)
	}

	tests := []struct {
		name     string
		launchID string
		wantFlag [2]string // a flag and the value that follows it in argv
	}{
		{
			name:     "claude carries --permission-mode",
			launchID: "tether-claude",
			wantFlag: [2]string{"--permission-mode", "bypassPermissions"},
		},
		{
			name:     "codex carries approval_policy",
			launchID: "agent-mux-codex-launch",
			wantFlag: [2]string{"-c", `approval_policy="on-request"`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !liveLaunchPresent(catalogRoot, tc.launchID) {
				t.Skipf("launch %q is not in the live catalog %s (host drift, not a failure)", tc.launchID, catalogRoot)
			}
			plan, err := res.Resolve(tc.launchID, agentlaunch.PolicyError)
			if err != nil {
				t.Fatalf("Resolve(%s): %v", tc.launchID, err)
			}

			workspace := t.TempDir()
			bootRoot := filepath.Join(workspace, "boot")
			if err := os.MkdirAll(bootRoot, 0o750); err != nil {
				t.Fatalf("mkdir boot root: %v", err)
			}
			plan.Workspace.WorkspaceDir = workspace
			plan.Workspace.TempPrefix = bootRoot

			ctx := context.Background()
			compiled, err := launcher.Compile(ctx, plan)
			if err != nil {
				t.Fatalf("Compile(%s): %v", tc.launchID, err)
			}
			prepared, err := launcher.Prepare(ctx, compiled)
			if err != nil {
				t.Fatalf("Prepare(%s): %v", tc.launchID, err)
			}
			// No WithAdapter — exercises providerplant.DefaultResolver,
			// the same planting path the daemon session-launch uses.
			if err := providerplant.Plant(ctx, prepared); err != nil {
				t.Fatalf("Plant(%s): %v", tc.launchID, err)
			}

			ok := false
			for i := 0; i+1 < len(prepared.Argv); i++ {
				if prepared.Argv[i] == tc.wantFlag[0] && prepared.Argv[i+1] == tc.wantFlag[1] {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("prepared argv does not carry the approval posture %s %s: %q", tc.wantFlag[0], tc.wantFlag[1], prepared.Argv)
			} else {
				t.Logf("%s: prepared argv carries the approval posture", tc.launchID)
			}
		})
	}
}
