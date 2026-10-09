package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/setup"
	"github.com/hollis-labs/tether/internal/workspace"
)

// A generated Claude boot directory must be writable without opening the
// control plane. A legacy project session_root still needs an explicit owned
// launch write_home; the guard must keep refusing it otherwise.
func TestWorkspaceParity_GeneratedBootLocation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{"XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME"} {
		t.Setenv(key, filepath.Join(home, key))
	}
	t.Setenv("TETHER_LAUNCH_ENGINE", "catalog")
	root := filepath.Join(home, ".tether")
	if _, err := setup.WriteCatalog(root, setup.WriteOpts{Minimal: true, StateRoot: root}); err != nil {
		t.Fatal(err)
	}
	cat, err := config.LoadLayered(filepath.Join(root, "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(home, "project")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	cat.Agents["agent"] = config.Agent{ID: "agent"}
	provider := cat.Providers["claude-code"]
	provider.Command = "/bin/true" // Plant only; no provider process is started.
	cat.Providers["claude-code"] = provider
	prev := bwrapAvailable
	bwrapAvailable = func(string) error { return nil }
	t.Cleanup(func() { bwrapAvailable = prev })
	svc := &Service{CatalogRoot: filepath.Join(root, "catalog"), Catalog: cat, protectionStatus: protectionOnForTest}

	for _, tc := range []struct {
		name        string
		sessionRoot string
		writeHome   string
		refused     bool
	}{
		{name: "seed-default"},
		{name: "owned-launch-override", sessionRoot: filepath.Join(root, "workspaces", "project"), writeHome: filepath.Join(home, "owned-workspaces")},
		{name: "legacy-protected-project", sessionRoot: filepath.Join(root, "workspaces", "project"), refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat.Projects["project"] = config.Project{ID: "project", RepoRoot: repo, Workspace: config.WorkspaceSpec{DefaultMode: "hybrid", SessionRoot: tc.sessionRoot}}
			cat.Launches["launch"] = config.Launch{ID: "launch", Project: "project", Agent: "agent", Provider: "claude-code", Workspace: config.LaunchWorkspace{WriteHome: tc.writeHome}}
			plan, err := launch.Resolve(cat, launch.Input{LaunchID: "launch", CatalogRoot: svc.CatalogRoot})
			if err != nil {
				t.Fatal(err)
			}
			if err := workspace.MaterializeWorkRoot(plan.WriteHome, tc.name, plan); err != nil {
				t.Fatal(err)
			}
			ws, err := workspace.Create(plan.WriteHome, tc.name, plan)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := svc.prepareSharedLaunch(context.Background(), plan, ws.Root, plantContextInput{ArtifactAdmission: testArtifactAdmission(t, plan)})
			if err != nil {
				t.Fatal(err)
			}
			opts := &agentsessions.StartOptions{Workdir: prepared.Workdir, WorkspaceDir: ws.Root}
			err = svc.applyControlPlaneProtection(plan, "cli", opts)
			if tc.refused {
				if !errors.Is(err, launch.ErrLaunchInsideProtectedPath) {
					t.Fatalf("protected project: got %v, want protected-path refusal", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("generated boot location refused: %v", err)
			}
			if len(opts.ProtectedPaths) == 0 {
				t.Fatal("launch lost control-plane protection")
			}
			if pathWithin(root, prepared.PlantedBootDir) || pathWithin(root, prepared.Workdir) {
				t.Fatalf("boot location remains in protected root: boot=%s cwd=%s", prepared.PlantedBootDir, prepared.Workdir)
			}
		})
	}
}
