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
	if err := os.MkdirAll(filepath.Dir(workRoot), 0o750); err != nil {
		return fmt.Errorf("create worktree parent: %w", err)
	}

	if err := runGit(plan.RepoRoot, "worktree", "add", "--detach", workRoot, "HEAD"); err != nil {
		return fmt.Errorf("create git worktree %s from %s: %w", workRoot, plan.RepoRoot, err)
	}
	plan.WorkRoot = workRoot
	return nil
}

// RemoveMaterializedWorkRoot removes a worktree created by MaterializeWorkRoot.
// Shared/hybrid launches are left untouched.
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
	if err := runGit(plan.RepoRoot, "worktree", "remove", "--force", plan.WorkRoot); err != nil {
		return fmt.Errorf("remove git worktree %s: %w", plan.WorkRoot, err)
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
