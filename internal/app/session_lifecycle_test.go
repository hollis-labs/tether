package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
)

// initGitRepoForTest creates a one-commit git repo and returns its path.
func initGitRepoForTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test User"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	for _, args := range [][]string{
		{"add", "README.md"},
		{"commit", "-m", "initial"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func worktreeListed(t *testing.T, repo, workRoot string) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "worktree", "list")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git worktree list: %v\n%s", err, out)
	}
	return strings.Contains(string(out), workRoot)
}

// TestCreateSessionFromPlan_FailedCreate_RemovesWorktree pins the
// cleanup-on-error contract: when a step after MaterializeWorkRoot fails
// (here, an unknown provider with no runtime factory), the worktree the call
// materialized must be removed so a failed create does not leak a worktree.
func TestCreateSessionFromPlan_FailedCreate_RemovesWorktree(t *testing.T) {
	repo := initGitRepoForTest(t)
	wsRoot := t.TempDir()

	// Empty factories map → the provider lookup fails after the worktree is
	// already materialized, exercising the cleanup defer.
	svc := &Service{factories: map[string]RuntimeFactory{}}

	plan := &launch.Plan{
		LaunchID:      "test-launch",
		ProjectID:     "demo",
		ProviderID:    "no-such-provider",
		RepoRoot:      repo,
		WriteHome:     wsRoot,
		WorkspaceMode: "worktree",
	}

	_, err := svc.createSessionFromPlan(plan)
	if err == nil {
		t.Fatal("expected createSessionFromPlan to fail, got nil")
	}
	if !strings.Contains(err.Error(), "no runtime for provider") {
		t.Fatalf("unexpected error: %v", err)
	}

	// work_root was set during materialization; the directory must be gone.
	if plan.WorkRoot == "" {
		t.Fatal("plan.WorkRoot never set — materialization did not run")
	}
	if _, statErr := os.Stat(plan.WorkRoot); !os.IsNotExist(statErr) {
		t.Fatalf("leaked worktree dir after failed create, stat err=%v", statErr)
	}
	if worktreeListed(t, repo, plan.WorkRoot) {
		t.Fatal("leaked git worktree registration after failed create")
	}
}

// TestCreateSessionFromPlan_FailedCreate_SharedModeKeepsRepoRoot guards the
// shared-mode invariant: a failed create in shared mode must never remove the
// repo_root, since shared launches do not materialize a worktree.
func TestCreateSessionFromPlan_FailedCreate_SharedModeKeepsRepoRoot(t *testing.T) {
	repo := initGitRepoForTest(t)
	wsRoot := t.TempDir()

	svc := &Service{factories: map[string]RuntimeFactory{}}
	plan := &launch.Plan{
		LaunchID:      "test-launch",
		ProjectID:     "demo",
		ProviderID:    "no-such-provider",
		RepoRoot:      repo,
		WriteHome:     wsRoot,
		WorkspaceMode: "shared",
	}

	if _, err := svc.createSessionFromPlan(plan); err == nil {
		t.Fatal("expected createSessionFromPlan to fail, got nil")
	}
	if plan.WorkRoot != repo {
		t.Fatalf("shared mode WorkRoot = %q, want repo_root %q", plan.WorkRoot, repo)
	}
	if _, statErr := os.Stat(repo); statErr != nil {
		t.Fatalf("shared-mode failed create removed repo_root: %v", statErr)
	}
}
