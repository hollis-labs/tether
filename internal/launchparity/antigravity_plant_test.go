package launchparity

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hollis-labs/substrate/harness/agentlaunch"
	"github.com/hollis-labs/substrate/harness/agentlaunch/launcher"
	providerplant "github.com/hollis-labs/substrate/harness/agentlaunch/planting"
	"github.com/hollis-labs/substrate/llm-core/contracts/runtimes"

	"github.com/hollis-labs/tether/internal/launchresolve"
	"github.com/hollis-labs/tether/internal/specresolve"
)

// TestPlantSmoke_AntigravitySpecPath drives an agy launch through the spec
// path the daemon uses — specresolve.Resolve -> launcher.Compile -> Prepare
// -> providerplant.Plant (DefaultResolver) — offline, against the
// specresolve fixture catalog and a minimum-config bag written to a
// temporary corpus. The live corpus is not touched: its bags are compared
// with the live catalog by TestFullCorpusParity, which has no agy launch.
//
// It asserts the agy contract survives into the planted boot dir: the
// workspace .agents plugin carrying MCP, AGENTS.md, cwd = boot dir, the
// project attached with --add-dir, and no HOME relocation.
func TestPlantSmoke_AntigravitySpecPath(t *testing.T) {
	t.Setenv("TESSERACT_URL", "") // offline: call vars degrade under on_error: warn

	specs := t.TempDir()
	assembly, err := os.ReadFile(filepath.Join(specsRoot, "launch-assembly.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(specs, "launch-assembly.yaml"), assembly, 0o600); err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(specs, "launches"), 0o750); err != nil {
		t.Fatal(err)
	}
	bag := "spec: tether.launch\nname: agy-minimum\ninputs:\n  work_dir: " + project + "\n  runner: antigravity\n"
	if err := os.WriteFile(filepath.Join(specs, "launches", "agy-minimum.yaml"), []byte(bag), 0o600); err != nil {
		t.Fatal(err)
	}

	reg, err := launchresolve.OpenAt(launchresolve.Options{CatalogRoot: filepath.Join("..", "specresolve", "testdata", "catalog")})
	if err != nil {
		t.Fatalf("launchresolve.OpenAt: %v", err)
	}
	res, err := specresolve.NewResolver(reg, specresolve.WithSpecsRoot(specs))
	if err != nil {
		t.Fatalf("specresolve.NewResolver: %v", err)
	}
	plan, err := res.Resolve("agy-minimum", agentlaunch.PolicyError)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.Provider.ID != "antigravity" || plan.Runtime != runtimes.ModeSubprocessPerTurn {
		t.Fatalf("provider/runtime = %q/%q; want antigravity/subprocess", plan.Provider.ID, plan.Runtime)
	}

	workspace := t.TempDir()
	bootRoot := filepath.Join(workspace, "boot")
	if err := os.MkdirAll(bootRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	plan.Workspace.WorkspaceDir = workspace
	plan.Workspace.TempPrefix = bootRoot

	ctx := context.Background()
	compiled, err := launcher.Compile(ctx, plan)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	prepared, err := launcher.Prepare(ctx, compiled)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	projectRoot := prepared.Workdir
	if err := providerplant.Plant(ctx, prepared); err != nil {
		t.Fatalf("Plant: %v", err)
	}

	boot := prepared.PlantedBootDir
	for _, f := range []string{"AGENTS.md", "boot.md", ".agents/plugins/tether/plugin.json", ".agents/plugins/tether/mcp_config.json"} {
		if _, err := os.Stat(filepath.Join(boot, f)); err != nil {
			t.Errorf("planted %s missing: %v", f, err)
		}
	}
	if prepared.Workdir != boot {
		t.Errorf("Workdir = %q; want the boot dir %q", prepared.Workdir, boot)
	}
	if i := slices.Index(prepared.Argv, "--add-dir"); i < 0 || i+1 >= len(prepared.Argv) || prepared.Argv[i+1] != projectRoot {
		t.Errorf("argv lacks --add-dir <project>: %v", prepared.Argv)
	}
	if _, relocated := prepared.Env["HOME"]; relocated {
		t.Errorf("antigravity launch relocates HOME: %v", prepared.Env["HOME"])
	}
}
