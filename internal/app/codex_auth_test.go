package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/substrate/harness/agentlaunch"
	"github.com/hollis-labs/substrate/harness/workspace"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

// fixtureAuth is a stand-in login; no test reads a real credential.
const fixtureAuth = `{"auth_mode":"fixture","OPENAI_API_KEY":"not-a-real-key"}`

func prepareCodexLaunch(t *testing.T, providerID, brand, runtimeKind string) *agentlaunch.PreparedLaunch {
	t.Helper()
	svc := &Service{
		CatalogRoot: t.TempDir(),
		Catalog:     &config.Catalog{Global: config.Global{Version: "test"}},
	}
	ws := t.TempDir()
	plan := &launch.Plan{
		LaunchID:       "demo",
		ProjectID:      "project",
		LogicalAgentID: "agent",
		ProviderID:     providerID,
		ProviderBrand:  brand,
		RuntimeKind:    runtimeKind,
		RepoRoot:       t.TempDir(),
		WriteHome:      ws,
		WorkspaceMode:  "shared",
		Command:        brand,
		BootPrompt:     "boot",
	}
	prepared, err := svc.prepareSharedLaunch(context.Background(), plan, ws, plantContextInput{ArtifactAdmission: testArtifactAdmission(t, plan), TetherCommand: "tether"})
	if err != nil {
		t.Fatalf("prepareSharedLaunch: %v", err)
	}
	return prepared
}

func hostCodexHome(t *testing.T, loggedIn bool) string {
	t.Helper()
	dir := t.TempDir()
	if loggedIn {
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(fixtureAuth), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CODEX_HOME", dir)
	return dir
}

func plantedCodexAuth(t *testing.T, prepared *agentlaunch.PreparedLaunch) string {
	t.Helper()
	codexHome := prepared.Env["CODEX_HOME"]
	if codexHome == "" {
		t.Fatal("codex launch has no CODEX_HOME in its env")
	}
	if !strings.HasPrefix(codexHome, prepared.PlantedBootDir) {
		t.Fatalf("CODEX_HOME %q is not in the boot dir %q", codexHome, prepared.PlantedBootDir)
	}
	return filepath.Join(codexHome, "auth.json")
}

func TestPrepareSharedLaunch_CodexLinksHostAuth(t *testing.T) {
	for _, tc := range []struct{ providerID, runtimeKind string }{
		{"codex-cli", config.RuntimeKindSubprocess},
		{"codex-app-server", config.RuntimeKindJSONRPCStdio},
	} {
		t.Run(tc.providerID, func(t *testing.T) {
			host := hostCodexHome(t, true)
			planted := plantedCodexAuth(t, prepareCodexLaunch(t, tc.providerID, "codex", tc.runtimeKind))

			info, err := os.Lstat(planted)
			if err != nil {
				t.Fatalf("lstat planted auth.json: %v", err)
			}
			if info.Mode()&fs.ModeSymlink == 0 {
				t.Fatalf("planted auth.json is %v, want a symlink to the host login", info.Mode())
			}
			target, err := os.Readlink(planted)
			if err != nil {
				t.Fatal(err)
			}
			if want := filepath.Join(host, "auth.json"); target != want {
				t.Fatalf("auth.json -> %q, want %q", target, want)
			}
			got, err := os.ReadFile(planted) //nolint:gosec // test-owned fixture
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != fixtureAuth {
				t.Fatal("planted auth.json does not read the host login")
			}
		})
	}
}

func TestCaptureCodexHome_RequiresExplicitExistingSource(t *testing.T) {
	for _, missing := range []string{"unset", "absent", "directory", "symlink"} {
		t.Run(missing, func(t *testing.T) {
			home := hostCodexHome(t, false)
			auth := filepath.Join(home, "auth.json")
			switch missing {
			case "unset":
				t.Setenv("CODEX_HOME", "")
			case "directory":
				if err := os.Mkdir(auth, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				source := filepath.Join(t.TempDir(), "auth.json")
				if err := os.WriteFile(source, []byte(fixtureAuth), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(source, auth); err != nil {
					t.Fatal(err)
				}
			}
			captured, err := captureCodexHome()
			if captured != nil {
				_ = captured.Close()
			}
			if err == nil {
				t.Fatal("uncaptured/nonregular source accepted")
			}
		})
	}
}

func TestPrepareSharedLaunch_ClaudeLinksNoCodexAuth(t *testing.T) {
	hostCodexHome(t, true)
	prepared := prepareCodexLaunch(t, "claude-code", "claude", config.RuntimeKindStreamingStdio)
	err := filepath.WalkDir(prepared.PlantedBootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			t.Errorf("claude boot dir has a symlink: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPrepareSharedLaunch_CodexMissingSourceRefusesBeforePrepare(t *testing.T) {
	hostCodexHome(t, false)
	dir := t.TempDir()
	prepared, err := (&Service{}).prepareSharedLaunch(context.Background(), &launch.Plan{ProviderBrand: "codex"}, dir, plantContextInput{})
	var refused *workspace.Refusal
	if prepared != nil || !errors.As(err, &refused) || refused.Code != "codex_existing_auth_source_required" {
		t.Fatalf("missing explicit source: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "boot")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing source allocated a boot directory: %v", err)
	}
}
