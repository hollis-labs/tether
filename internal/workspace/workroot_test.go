package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	if out := gitOutput(t, repo, "branch", "--list", "tether/*"); out != "" {
		t.Fatalf("worktree launch created local tether branch: %s", out)
	}
	if err := RemoveMaterializedWorkRoot(plan); err != nil {
		t.Fatalf("RemoveMaterializedWorkRoot: %v", err)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("worktree dir still exists after cleanup, stat err=%v", err)
	}
}

func TestMaterializeWorkRootCollisionSurfacesClearError(t *testing.T) {
	repo := initGitRepo(t)
	root := t.TempDir()

	plan := &launch.Plan{RepoRoot: repo, WorkspaceMode: "worktree"}
	if err := MaterializeWorkRoot(root, "session-collide", plan); err != nil {
		t.Fatalf("first MaterializeWorkRoot: %v", err)
	}

	// A second materialize with the same run id collides on the existing path.
	dup := &launch.Plan{RepoRoot: repo, WorkspaceMode: "worktree"}
	err := MaterializeWorkRoot(root, "session-collide", dup)
	if err == nil {
		t.Fatal("expected collision error, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("collision error not actionable: %v", err)
	}
	if dup.WorkRoot != "" {
		t.Fatalf("colliding plan got a WorkRoot set: %q", dup.WorkRoot)
	}
}

func TestMaterializeWorkRootRegisteredPathSurfacesClearError(t *testing.T) {
	repo := initGitRepo(t)
	root := t.TempDir()

	plan := &launch.Plan{RepoRoot: repo, WorkspaceMode: "worktree"}
	if err := MaterializeWorkRoot(root, "session-reg", plan); err != nil {
		t.Fatalf("MaterializeWorkRoot: %v", err)
	}
	// Delete the directory but leave the git worktree registration intact —
	// `git worktree add` to the same path must still fail with a framed error.
	if err := os.RemoveAll(plan.WorkRoot); err != nil {
		t.Fatalf("remove worktree dir: %v", err)
	}
	dup := &launch.Plan{RepoRoot: repo, WorkspaceMode: "worktree"}
	err := MaterializeWorkRoot(root, "session-reg", dup)
	if err == nil {
		t.Fatal("expected registered-path error, got nil")
	}
	if !strings.Contains(err.Error(), "worktree") {
		t.Fatalf("registered-path error not framed: %v", err)
	}
}

func TestMaterializeWorkRootNamedBranch(t *testing.T) {
	repo := initGitRepo(t)
	root := t.TempDir()

	plan := &launch.Plan{RepoRoot: repo, WorkspaceMode: "worktree", WorktreeName: "tether-session-abc"}
	if err := MaterializeWorkRoot(root, "session-named", plan); err != nil {
		t.Fatalf("MaterializeWorkRoot: %v", err)
	}
	if out := gitOutput(t, repo, "branch", "--list", "tether-session-abc"); !strings.Contains(out, "tether-session-abc") {
		t.Fatalf("named worktree did not create branch: %q", out)
	}
	if err := RemoveMaterializedWorkRoot(plan); err != nil {
		t.Fatalf("RemoveMaterializedWorkRoot: %v", err)
	}
}

func TestMaterializeWorkRootTemplateNameStaysDetached(t *testing.T) {
	repo := initGitRepo(t)
	root := t.TempDir()

	// An unrendered template must NOT be fed to git as a branch name.
	plan := &launch.Plan{
		RepoRoot:      repo,
		WorkspaceMode: "worktree",
		WorktreeName:  "tether/{{.ProjectID}}/{{.SessionID}}",
	}
	if err := MaterializeWorkRoot(root, "session-tmpl", plan); err != nil {
		t.Fatalf("MaterializeWorkRoot: %v", err)
	}
	if out := gitOutput(t, repo, "branch", "--list", "tether/*"); out != "" {
		t.Fatalf("template worktree_name leaked a branch: %q", out)
	}
}

func TestRemoveWorktreeAtDeregistersRegistration(t *testing.T) {
	repo := initGitRepo(t)
	root := t.TempDir()

	plan := &launch.Plan{RepoRoot: repo, WorkspaceMode: "worktree"}
	if err := MaterializeWorkRoot(root, "session-dereg", plan); err != nil {
		t.Fatalf("MaterializeWorkRoot: %v", err)
	}
	if err := RemoveWorktreeAt(repo, plan.WorkRoot); err != nil {
		t.Fatalf("RemoveWorktreeAt: %v", err)
	}
	if out := gitOutput(t, repo, "worktree", "list"); strings.Contains(out, plan.WorkRoot) {
		t.Fatalf("worktree still registered after RemoveWorktreeAt: %q", out)
	}
	// Idempotent: removing an already-gone worktree must not error.
	if err := RemoveWorktreeAt(repo, plan.WorkRoot); err != nil {
		t.Fatalf("second RemoveWorktreeAt: %v", err)
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

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
