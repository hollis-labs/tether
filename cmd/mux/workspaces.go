package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/chrispian/agent-mux/internal/config"
	"github.com/chrispian/agent-mux/internal/store"
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
pruned. Pass --dry-run to preview without deleting.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cat, err := config.Load(catalogPath)
		if err != nil {
			return err
		}
		wsRoot := config.Expand(cat.Global.Catalog.Defaults.WorkspaceRoot)
		if wsRoot == "" {
			return fmt.Errorf("workspace_root not configured in catalog")
		}
		dbPath := config.Expand(cat.Global.Catalog.Defaults.StateDB)
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
					} else {
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

func init() {
	workspacesPruneCmd.Flags().StringVar(&pruneOlderThan, "older-than", "24h",
		"prune workspaces for sessions last updated more than this duration ago (e.g. 24h, 7d)")
	workspacesPruneCmd.Flags().BoolVar(&pruneDryRun, "dry-run", false,
		"preview what would be removed without deleting")
	workspacesCmd.AddCommand(workspacesPruneCmd)
}
