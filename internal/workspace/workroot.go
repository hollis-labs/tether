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

	branch := worktreeBranchName(plan, runID)
	if err := runGit(plan.RepoRoot, "worktree", "add", "-b", branch, workRoot, "HEAD"); err != nil {
		return fmt.Errorf("create git worktree %s from %s: %w", workRoot, plan.RepoRoot, err)
	}
	plan.WorkRoot = workRoot
	return nil
}

func worktreeBranchName(plan *launch.Plan, runID string) string {
	name := plan.WorktreeName
	if name == "" {
		name = "tether/{{.ProjectID}}/{{.AgentID}}/{{.SessionID}}"
	}
	repl := map[string]string{
		"{{.SessionID}}": runID,
		"{{.RunID}}":     runID,
		"{{.ProjectID}}": plan.ProjectID,
		"{{.LaunchID}}":  plan.LaunchID,
		"{{.AgentID}}":   plan.LogicalAgentID,
	}
	for k, v := range repl {
		name = strings.ReplaceAll(name, k, v)
	}
	return sanitizeGitBranch(name)
}

func sanitizeGitBranch(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	lastSlash := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
			lastSlash = false
		case r == '/':
			if !lastSlash {
				b.WriteRune(r)
				lastSlash = true
			}
		default:
			b.WriteRune('-')
			lastSlash = false
		}
	}
	out := strings.Trim(b.String(), "/.-")
	out = strings.ReplaceAll(out, "..", "-")
	if out == "" {
		return "tether/worktree"
	}
	return out
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
