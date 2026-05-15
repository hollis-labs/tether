package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
)

func TestMaterializeWorkRootCreatesIsolatedGitWorktree(t *testing.T) {
	repo := initGitRepo(t)
	root := t.TempDir()

	plan := &launch.Plan{
		LaunchID:       "fragments-engine-claude-tui",
		ProjectID:      "fragments-engine",
		LogicalAgentID: "general",
		RepoRoot:       repo,
		WorkspaceMode:  "worktree",
	}
	if err := MaterializeWorkRoot(root, "session-123", plan); err != nil {
		t.Fatalf("MaterializeWorkRoot: %v", err)
	}

	want := filepath.Join(root, "session-123", "repo")
	if plan.WorkRoot != want {
		t.Fatalf("WorkRoot = %q, want %q", plan.WorkRoot, want)
	}
	if _, err := os.Stat(filepath.Join(want, ".git")); err != nil {
		t.Fatalf("worktree .git missing: %v", err)
	}

	writeFile(t, filepath.Join(want, "agent.txt"), "agent edit\n")
	if _, err := os.Stat(filepath.Join(repo, "agent.txt")); !os.IsNotExist(err) {
		t.Fatalf("agent edit leaked into source repo, stat err=%v", err)
	}
}

func TestMaterializeWorkRootSharedUsesRepoRoot(t *testing.T) {
	plan := &launch.Plan{RepoRoot: "/repo", WorkspaceMode: "hybrid"}
	if err := MaterializeWorkRoot("/workspaces", "session-123", plan); err != nil {
		t.Fatalf("MaterializeWorkRoot: %v", err)
	}
	if plan.WorkRoot != "/repo" {
		t.Fatalf("WorkRoot = %q, want /repo", plan.WorkRoot)
	}
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "git", "init")
	run(t, dir, "git", "config", "user.email", "test@example.com")
	run(t, dir, "git", "config", "user.name", "Test User")
	writeFile(t, filepath.Join(dir, "README.md"), "# demo\n")
	run(t, dir, "git", "add", "README.md")
	run(t, dir, "git", "commit", "-m", "initial")
	return dir
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
