package app

import (
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

func TestCodexThreadStartParamsCarriesWorkRoot(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	const workRoot = "/tmp/tether-worktree"
	row := store.SessionRow{
		ID:             "sess-codex",
		LaunchID:       "launch-codex",
		ProjectID:      "project",
		LogicalAgentID: "agent",
		ProviderID:     "codex-app-server",
		Workspace:      "/tmp/tether-session",
		State:          "created",
	}
	plan := &launch.Plan{
		LaunchID: "launch-codex",
		RepoRoot: "/tmp/tether",
		WorkRoot: workRoot,
	}
	if err := db.CreateSession(row, plan); err != nil {
		t.Fatalf("create session: %v", err)
	}

	svc := &Service{Store: db}
	params := svc.codexThreadStartParams("sess-codex")
	if got := params["cwd"]; got != workRoot {
		t.Fatalf("thread/start cwd = %v, want %q", got, workRoot)
	}
}

func TestCodexThreadStartParamsFallsBackToRepoRoot(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	const repoRoot = "/tmp/tether"
	row := store.SessionRow{
		ID:             "sess-codex",
		LaunchID:       "launch-codex",
		ProjectID:      "project",
		LogicalAgentID: "agent",
		ProviderID:     "codex-app-server",
		Workspace:      "/tmp/tether-session",
		State:          "created",
	}
	plan := &launch.Plan{
		LaunchID: "launch-codex",
		RepoRoot: repoRoot,
	}
	if err := db.CreateSession(row, plan); err != nil {
		t.Fatalf("create session: %v", err)
	}

	svc := &Service{Store: db}
	params := svc.codexThreadStartParams("sess-codex")
	if got := params["cwd"]; got != repoRoot {
		t.Fatalf("thread/start cwd = %v, want %q", got, repoRoot)
	}
}
