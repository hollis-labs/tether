package launchparity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
// approval/permission contract survives the flip into the *planted*
// boot-dir content:
//
//   - claude: planted .claude/settings.json carries permissions.defaultMode
//     (catalog default permission_mode: bypass -> "bypassPermissions").
//   - codex:  planted config.toml carries an approval_policy.
//
// It does NOT execute an agent — that is the live half of the smoke. This
// half is what the orchestrator-s5 amendment is really about: a launch
// that shows parity-green must not silently drop the boot-dir approval
// posture. providerplant.Plant here runs with no WithAdapter, so it goes
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
		wantFile string   // planted file, relative to the boot dir
		wantAny  []string // the planted file must contain at least one
	}{
		{
			name:     "claude carries permissions.defaultMode",
			launchID: "tether-claude",
			wantFile: ".claude/settings.json",
			wantAny:  []string{"bypassPermissions"},
		},
		{
			name:     "codex carries approval_policy",
			launchID: "agent-mux-codex-launch",
			wantFile: "config.toml",
			wantAny:  []string{"approval_policy"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
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

			planted := filepath.Join(prepared.PlantedBootDir, tc.wantFile)
			body, err := os.ReadFile(planted)
			if err != nil {
				t.Fatalf("planted %s not found: %v", tc.wantFile, err)
			}
			ok := false
			for _, want := range tc.wantAny {
				if strings.Contains(string(body), want) {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("planted %s does not carry the approval posture %v\n--- content ---\n%s",
					tc.wantFile, tc.wantAny, body)
			} else {
				t.Logf("%s: planted %s carries the approval posture", tc.launchID, tc.wantFile)
			}
		})
	}
}
