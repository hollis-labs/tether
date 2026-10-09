package launchartifacts_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/substrate/harness/agentlaunch"
	"github.com/hollis-labs/substrate/harness/agentlaunch/launcher"
	providerplant "github.com/hollis-labs/substrate/harness/agentlaunch/planting"
	"github.com/hollis-labs/substrate/harness/workspace"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchartifacts"
	"github.com/hollis-labs/tether/internal/launchartifacts/testfixture"
)

func compileLaunch(t *testing.T, brand string) *agentlaunch.CompiledLaunch {
	t.Helper()
	plan := launch.AgentLaunchPlan(&launch.Plan{ProviderBrand: brand, ProviderID: brand, RuntimeKind: "subprocess", Command: brand,
		RepoRoot: t.TempDir(), WorkspaceMode: "shared", BootPrompt: "owned boot prompt"}, t.TempDir())
	plan.Workspace.TempPrefix = t.TempDir()
	compiled, err := launcher.Compile(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func TestArtifactCustody_PlantsWithDurableEvidence(t *testing.T) {
	compiled := compileLaunch(t, "claude")
	prepared, custody, err := launchartifacts.Prepare(context.Background(), compiled, testfixture.Admission(t, func() any { return compiled }))
	if err != nil {
		t.Fatal(err)
	}
	defer custody.Close()
	if err := providerplant.Plant(context.Background(), prepared, providerplant.WithArtifactAuthorization(custody.Authorize)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(prepared.PlantedBootDir, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(prepared.PlantedBootDir, ".materialize", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := custody.Authorize(context.Background(), prepared.PlantedBootDir); err == nil {
		t.Fatal("used artifact custody accepted again")
	}
	if err := custody.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(prepared.PlantedBootDir); err != nil {
		t.Fatalf("closing custody removed boot evidence: %v", err)
	}
}

func TestArtifactCustody_RefusesChangedAuthorityBeforeRender(t *testing.T) {
	for _, change := range []string{"target", "directory", "mode", "plan", "operation", "closed"} {
		t.Run(change, func(t *testing.T) {
			compiled := compileLaunch(t, "claude")
			admission := testfixture.Admission(t, func() any { return compiled })
			prepared, custody, err := launchartifacts.Prepare(context.Background(), compiled, admission)
			if err != nil {
				t.Fatal(err)
			}
			defer custody.Close()
			target := prepared.PlantedBootDir
			ctx := context.Background()
			switch change {
			case "target":
				target = t.TempDir()
			case "directory":
				if err := os.Rename(target, target+"-held"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(target, 0750); err != nil {
					t.Fatal(err)
				}
			case "plan":
				compiled.Plan.Provider.Binary = "substituted"
			case "operation":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			case "closed":
				if err := custody.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := custody.Authorize(ctx, target); err == nil {
				t.Fatal("changed authority accepted")
			}
			if _, err := os.Stat(filepath.Join(prepared.PlantedBootDir, "CLAUDE.md")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refusal rendered artifacts: %v", err)
			}
		})
	}
}

func TestArtifactCustody_RechecksAuthorityAtApply(t *testing.T) {
	compiled := compileLaunch(t, "claude")
	admission := testfixture.Admission(t, func() any { return compiled })
	validate := admission.Validate
	accepted := true
	admission.Validate = func(ctx context.Context) error {
		if !accepted {
			return errors.New("accepted operation revoked")
		}
		return validate(ctx)
	}
	prepared, custody, err := launchartifacts.Prepare(context.Background(), compiled, admission)
	if err != nil {
		t.Fatal(err)
	}
	defer custody.Close()
	authority, err := custody.Authorize(context.Background(), prepared.PlantedBootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	accepted = false
	if err := authority.Ports.Host.Validate(context.Background(), workspace.Spec{OperationID: admission.OperationID, Operation: workspace.Prepare}, authority.Input.Resources); err == nil {
		t.Fatal("revoked operation accepted at apply")
	}
	if _, err := authority.Ports.Observations.Observe(context.Background(), authority.Input.Resources); err == nil {
		t.Fatal("revoked operation observed at apply")
	}
}

func TestArtifactCustody_RequiresAdmissionBeforeAllocation(t *testing.T) {
	compiled := compileLaunch(t, "claude")
	if _, _, err := launchartifacts.Prepare(context.Background(), compiled, launchartifacts.Admission{}); err == nil {
		t.Fatal("missing admission accepted")
	}
	entries, err := os.ReadDir(compiled.Plan.Workspace.TempPrefix)
	if err != nil || len(entries) != 0 {
		t.Fatalf("missing admission allocated a boot root: entries=%d err=%v", len(entries), err)
	}
}
