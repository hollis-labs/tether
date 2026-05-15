package workspace

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hollis-labs/tether/internal/launch"
)

const WorktreeMode = "worktree"

// MaterializeWorkRoot prepares the editable/exec root for a launch. Shared
// modes keep using repo_root; isolated modes create a per-launch git worktree.
//
// Workspace roots, in plain terms:
//
//   - repo_root      — the source checkout the launch derives from. Never
//     mutated or removed by the workspace package.
//   - work_root      — the editable/exec root the provider runs against. In
//     shared/hybrid mode this aliases repo_root; in worktree/isolated mode it
//     is the freshly materialized git worktree at <base>/repo.
//   - workspace_dir  — the per-session bookkeeping dir (logs, prompts, plan
//     JSON) created by workspace.Create; distinct from work_root.
//
// WorktreeName handling: if plan.WorktreeName is a non-empty literal git ref
// (no Go-template markers and no path separators) it is used as the branch
// name for `git worktree add -b <name>`. A template-style value such as
// "tether/{{.ProjectID}}/{{.SessionID}}" is treated as reserved/not-yet-wired
// — Tether has no template renderer for it today — and the worktree is created
// detached. An empty WorktreeName always means a detached worktree. This keeps
// the catalog field forward-compatible without silently feeding an unrendered
// template string to git.
func MaterializeWorkRoot(workspaceRoot, runID string, plan *launch.Plan) error {
	if plan == nil {
		return fmt.Errorf("launch plan required")
	}
	mode := strings.ToLower(strings.TrimSpace(plan.WorkspaceMode))
	switch mode {
	case "", "shared", "hybrid":
		plan.WorkRoot = plan.RepoRoot
		return nil
	case WorktreeMode, "isolated":
	default:
		return fmt.Errorf("unsupported workspace mode %q", plan.WorkspaceMode)
	}

	if plan.RepoRoot == "" {
		return fmt.Errorf("repo_root required for workspace mode %q", mode)
	}
	if runID == "" {
		return fmt.Errorf("run id required for workspace mode %q", mode)
	}

	base := plan.WorktreeBase
	if base == "" {
		if workspaceRoot == "" {
			return fmt.Errorf("workspace root required when worktree_base is empty")
		}
		base = filepath.Join(workspaceRoot, runID)
	}
	workRoot := filepath.Join(base, "repo")

	// Collision guard: refuse to clobber an existing path. A pre-existing
	// work_root means either a prior launch leaked it or two launches share a
	// runID — both deserve a clear, actionable error rather than a raw git
	// "already exists" message.
	if _, err := os.Stat(workRoot); err == nil {
		return fmt.Errorf("worktree path %s already exists — remove the stale worktree "+
			"(`git -C %s worktree remove --force %s` then `git -C %s worktree prune`) or pick a fresh run id",
			workRoot, plan.RepoRoot, workRoot, plan.RepoRoot)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat worktree path %s: %w", workRoot, err)
	}

	if err := os.MkdirAll(filepath.Dir(workRoot), 0o750); err != nil {
		return fmt.Errorf("create worktree parent: %w", err)
	}

	branch := worktreeBranchName(plan.WorktreeName)
	var gitArgs []string
	if branch != "" {
		gitArgs = []string{"worktree", "add", "-b", branch, workRoot, "HEAD"}
	} else {
		gitArgs = []string{"worktree", "add", "--detach", workRoot, "HEAD"}
	}
	if err := runGit(plan.RepoRoot, gitArgs...); err != nil {
		// `git worktree add` fails when the path or branch is already
		// registered. Frame the error so the operator knows the remedy
		// instead of seeing a bare git diagnostic.
		hint := ""
		if branch != "" {
			hint = fmt.Sprintf(" (branch %q may already exist — prune stale worktrees with `git -C %s worktree prune`)", branch, plan.RepoRoot)
		} else {
			hint = fmt.Sprintf(" (path may already be registered — prune stale worktrees with `git -C %s worktree prune`)", plan.RepoRoot)
		}
		return fmt.Errorf("create git worktree %s from %s%s: %w", workRoot, plan.RepoRoot, hint, err)
	}
	plan.WorkRoot = workRoot
	return nil
}

// worktreeBranchName returns the git branch name to use for a worktree, or ""
// when the worktree should be created detached. An unrendered Go template or a
// value containing a path separator is treated as reserved (detached) — see
// MaterializeWorkRoot's doc comment.
func worktreeBranchName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if strings.Contains(name, "{{") || strings.Contains(name, "}}") {
		return ""
	}
	if strings.ContainsAny(name, " \t") {
		return ""
	}
	return name
}

// RemoveMaterializedWorkRoot removes a worktree created by MaterializeWorkRoot.
// Shared/hybrid launches are left untouched.
//
// Retention policy: this is the explicit cleanup path. `boot-exec` calls it via
// defer so a one-shot exec leaves no worktree behind. Daemon-managed sessions
// do NOT call it on session teardown — a terminated session's worktree (and the
// work product / logs inside it) is preserved for inspection and is reclaimed
// only by an explicit operator action (`mux workspaces prune`). It is always
// safe to call: it removes only worktree/isolated-mode roots and never the
// shared/hybrid repo_root.
func RemoveMaterializedWorkRoot(plan *launch.Plan) error {
	if plan == nil || plan.WorkRoot == "" || plan.WorkRoot == plan.RepoRoot {
		return nil
	}
	mode := strings.ToLower(strings.TrimSpace(plan.WorkspaceMode))
	switch mode {
	case WorktreeMode, "isolated":
	default:
		return nil
	}
	return RemoveWorktreeAt(plan.RepoRoot, plan.WorkRoot)
}

// RemoveWorktreeAt removes a single git worktree registered against repoRoot.
// It is the path-level primitive behind RemoveMaterializedWorkRoot and is used
// by cleanup commands (e.g. `mux workspaces prune`) that have a worktree path
// but no launch plan. Using `git worktree remove` rather than a bare
// os.RemoveAll keeps the source repo's worktree registry consistent — a bare
// RemoveAll leaves a stale registration that later `git worktree add` calls
// trip over. A best-effort `git worktree prune` follows to clear the registry
// even when the directory was already gone.
func RemoveWorktreeAt(repoRoot, workRoot string) error {
	if repoRoot == "" || workRoot == "" {
		return nil
	}
	if err := runGit(repoRoot, "worktree", "remove", "--force", workRoot); err != nil {
		// The directory may already be gone; fall back to a prune so the
		// registry does not keep a dangling entry.
		_ = runGit(repoRoot, "worktree", "prune")
		if _, statErr := os.Stat(workRoot); os.IsNotExist(statErr) {
			return nil
		}
		return fmt.Errorf("remove git worktree %s: %w", workRoot, err)
	}
	return nil
}

func runGit(repoRoot string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", repoRoot}, args...)...) //nolint:gosec // repo path and args are operator/catalog controlled.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, msg)
	}
	return nil
}
