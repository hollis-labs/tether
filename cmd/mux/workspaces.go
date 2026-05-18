package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workspace"
)

var workspacesCmd = &cobra.Command{
	Use:   "workspaces",
	Short: "Session workspace commands",
}

var (
	pruneOlderThan string
	pruneDryRun    bool
)

var workspacesPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove workspace dirs for terminal sessions older than --older-than",
	Long: `Scans the workspace root and removes directories for sessions that are in a
terminal state (completed, failed, killed) and whose session was last updated
more than --older-than ago. Sessions not found in the database at all are also
pruned. Pass --dry-run to preview without deleting.

For worktree/isolated-mode sessions the prune first runs 'git worktree remove'
against the source repo so the repo's worktree registry stays consistent — a
bare directory delete would leave a stale worktree registration behind.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cat, err := config.Load(catalogPath)
		if err != nil {
			return err
		}
		// Explicit catalog values win; cat.Paths supplies the go-apppaths
		// fallback only when global.yaml omits the corresponding key.
		wsRoot := config.ResolveWorkspaceRoot(cat.Global.Catalog.Defaults, cat.Paths)
		if wsRoot == "" {
			return fmt.Errorf("workspace_root not configured in catalog")
		}
		dbPath := config.ResolveStateDB(cat.Global.Catalog.Defaults, cat.Paths)
		if dbPath == "" {
			return fmt.Errorf("state_db not configured in catalog")
		}

		threshold := 24 * time.Hour
		if pruneOlderThan != "" {
			threshold, err = time.ParseDuration(pruneOlderThan)
			if err != nil {
				return fmt.Errorf("invalid --older-than %q: %w", pruneOlderThan, err)
			}
		}
		cutoff := time.Now().UTC().Add(-threshold)

		db, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("open state db: %w", err)
		}
		defer func() { _ = db.Close() }()

		// Walk all project subdirs under the workspace root.
		// Layout: <workspace_root>/<project_id>/<session_uuid>/
		entries, err := os.ReadDir(wsRoot)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Fprintf(cmd.ErrOrStderr(), "workspace root %s does not exist; nothing to prune\n", wsRoot)
				return nil
			}
			return fmt.Errorf("read workspace root: %w", err)
		}

		var pruned, skipped int
		for _, projEntry := range entries {
			if !projEntry.IsDir() {
				continue
			}
			projDir := filepath.Join(wsRoot, projEntry.Name())
			sessEntries, err := os.ReadDir(projDir)
			if err != nil {
				continue
			}
			for _, sessEntry := range sessEntries {
				if !sessEntry.IsDir() {
					continue
				}
				sessID := sessEntry.Name()
				sessDir := filepath.Join(projDir, sessID)

				row, err := db.GetSession(sessID)
				shouldPrune := false
				reason := ""

				if err != nil {
					// Session not in DB — orphaned workspace.
					shouldPrune = true
					reason = "not in database"
				} else {
					terminal := row.State == "completed" || row.State == "failed" || row.State == "killed"
					if !terminal {
						skipped++
						continue
					}
					updatedAt, parseErr := time.Parse(time.RFC3339, row.UpdatedAt)
					if parseErr != nil || updatedAt.Before(cutoff) {
						shouldPrune = true
						reason = fmt.Sprintf("state=%s updated_at=%s", row.State, row.UpdatedAt)
					} else {
						skipped++
						continue
					}
				}

				if shouldPrune {
					if pruneDryRun {
						fmt.Printf("would remove %s (%s)\n", sessDir, reason)
						if wt := worktreeFromSessionDir(sessDir); wt.workRoot != "" {
							fmt.Printf("  would deregister git worktree %s from %s\n", wt.workRoot, wt.repoRoot)
						}
					} else {
						// Deregister the git worktree first so the source
						// repo's worktree registry does not keep a stale
						// entry after the directory is removed.
						if wt := worktreeFromSessionDir(sessDir); wt.workRoot != "" {
							if err := workspace.RemoveWorktreeAt(wt.repoRoot, wt.workRoot); err != nil {
								fmt.Fprintf(cmd.ErrOrStderr(), "warning: deregister worktree %s: %v\n", wt.workRoot, err)
							}
						}
						if err := os.RemoveAll(sessDir); err != nil {
							fmt.Fprintf(cmd.ErrOrStderr(), "warning: remove %s: %v\n", sessDir, err)
						} else {
							fmt.Printf("removed %s (%s)\n", sessDir, reason)
						}
					}
					pruned++
				}
			}
		}

		if pruneDryRun {
			fmt.Printf("\ndry-run: would remove %d workspace(s), kept %d active\n", pruned, skipped)
		} else {
			fmt.Printf("\npruned %d workspace(s), kept %d active\n", pruned, skipped)
		}
		return nil
	},
}

// worktreeRef names the source repo and materialized work_root of a
// worktree/isolated-mode session.
type worktreeRef struct {
	repoRoot string
	workRoot string
}

// worktreeFromSessionDir inspects a session's persisted plan (state/plan.json)
// and returns the git worktree it materialized, or a zero worktreeRef when the
// session was not worktree-mode (shared/hybrid sessions have nothing to
// deregister). Any read/parse failure yields a zero value — the caller then
// falls back to a plain directory delete.
func worktreeFromSessionDir(sessDir string) worktreeRef {
	planPath := filepath.Join(sessDir, "state", "plan.json")
	raw, err := os.ReadFile(planPath) //nolint:gosec // path derived from the operator-configured workspace root.
	if err != nil {
		return worktreeRef{}
	}
	var plan launch.Plan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return worktreeRef{}
	}
	switch plan.WorkspaceMode {
	case workspace.WorktreeMode, "isolated":
	default:
		return worktreeRef{}
	}
	if plan.WorkRoot == "" || plan.WorkRoot == plan.RepoRoot || plan.RepoRoot == "" {
		return worktreeRef{}
	}
	return worktreeRef{repoRoot: plan.RepoRoot, workRoot: plan.WorkRoot}
}

func init() {
	workspacesPruneCmd.Flags().StringVar(&pruneOlderThan, "older-than", "24h",
		"prune workspaces for sessions last updated more than this duration ago (e.g. 24h, 7d)")
	workspacesPruneCmd.Flags().BoolVar(&pruneDryRun, "dry-run", false,
		"preview what would be removed without deleting")
	workspacesCmd.AddCommand(workspacesPruneCmd)
}
