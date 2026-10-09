package launchparity

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/substrate/harness/agentlaunch"
	"github.com/hollis-labs/substrate/harness/agentlaunch/launcher"
	"github.com/hollis-labs/substrate/harness/agentlaunch/matrix"
	"github.com/hollis-labs/substrate/harness/agentlaunch/parity"
	providerplant "github.com/hollis-labs/substrate/harness/agentlaunch/planting"
	"github.com/hollis-labs/tether/internal/launchartifacts"
	"github.com/hollis-labs/tether/internal/launchartifacts/testfixture"

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
//   - codex:  -c approval_policy="never" (explicit bypass posture yolo,
//     paired with danger-full-access).
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
			launchID: "tether-codex-launch",
			wantFlag: [2]string{"-c", `approval_policy="never"`},
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
			prepared, custody, err := launchartifacts.Prepare(ctx, compiled, testfixture.Admission(t, func() any { return plan }))
			if err != nil {
				t.Fatalf("Prepare(%s): %v", tc.launchID, err)
			}
			defer custody.Close()
			// No WithAdapter — exercises providerplant.DefaultResolver,
			// the same planting path the daemon session-launch uses.
			descriptor, err := matrix.Lookup(plan.Provider, plan.Runtime)
			if err != nil {
				t.Fatal(err)
			}
			if string(descriptor.ProviderID) == "codex" {
				// A literal owned stand-in exercises DEC-036 without discovering
				// or linking any credential from this process's environment.
				source := t.TempDir()
				if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte(`{"fixture":true}`), 0600); err != nil {
					t.Fatal(err)
				}
				home, err := launchartifacts.CaptureCodexHome(source)
				if err != nil {
					t.Fatal(err)
				}
				defer home.Close()
				if err := custody.PlantCodex(ctx, prepared, home); err != nil {
					t.Fatalf("Plant(%s): %v", tc.launchID, err)
				}
			} else if err := providerplant.Plant(ctx, prepared, providerplant.WithArtifactAuthorization(custody.Authorize)); err != nil {
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
