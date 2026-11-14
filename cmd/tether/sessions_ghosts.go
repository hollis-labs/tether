package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/app"
)

// sessions reap-ghosts (CW-20261114-0007): a dry run by default; --apply needs --backup.
func init() {
	var apply, dryRun, asJSON bool
	var backup string
	cmd := &cobra.Command{
		Use:   "reap-ghosts",
		Short: "Report (or, with --apply, clean up) ghost sessions, dead shim custody and stale workers",
		Long: `Classifies, against the hub's live state:
  sessions  open rows on a connected worker missing from its live inventory, on a worker gone 7 days,
            or hub-local rows the reaper orphaned with nothing alive behind them;
  custody   hub-local shim custody of ended sessions with no live process or socket;
  workers   rows not seen for 7 days with no open session (retired: hidden, never deleted).
Without --apply nothing is changed. --apply needs --backup naming a SQLite backup of the database
written within the last 2 hours.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if apply && dryRun {
				return fmt.Errorf("--apply and --dry-run are exclusive")
			}
			if apply && backup == "" {
				return fmt.Errorf("--apply needs --backup <path of the tether.db backup taken first>")
			}
			c, err := localDeviceAdminClient()
			if err != nil {
				return err
			}
			var report app.GhostReport
			if err = c.call(cmd.Context(), http.MethodPost, "/ghosts/reap", map[string]any{"apply": apply, "backup": backup}, &report); err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(report)
			}
			printGhostReport(cmd.OutOrStdout(), &report)
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "clean up (default: dry run)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report only (the default)")
	cmd.Flags().StringVar(&backup, "backup", "", "path of the database backup taken before --apply")
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	sessionsCmd.AddCommand(cmd)
}

func printGhostReport(w io.Writer, r *app.GhostReport) {
	mode := "DRY RUN (nothing changed)"
	if r.Applied {
		mode = "APPLIED (backup " + r.Backup + ")"
	}
	fmt.Fprintln(w, mode)
	fmt.Fprintln(w, "sessions:")
	for _, g := range r.Sessions {
		fmt.Fprintf(w, "  %s\t%s\t%s\tworker=%s\tagent=%s\tupdated=%s\tbindings=%d\t%s%s\n", g.ID, g.Class, g.State, g.Worker, g.LogicalAgentID, g.UpdatedAt, g.Bindings, g.Action, done(g.Done))
	}
	fmt.Fprintln(w, "custody:")
	for _, c := range r.Custody {
		fmt.Fprintf(w, "  %s\t%s\t%s%s\n", c.SessionID, c.State, c.Action, done(c.Done))
	}
	fmt.Fprintln(w, "workers:")
	for _, x := range r.Workers {
		fmt.Fprintf(w, "  %s\t%s\tlast_seen=%s\tsessions=%d\topen=%d\tgrants=%d\tsuperseded_by=%s\t%s%s\n", x.ID, x.Label, x.LastSeen, x.Sessions, x.Open, x.Grants, x.SupersededBy, x.Action, done(x.Done))
	}
	counts := func(name string, c app.GhostCounts) {
		fmt.Fprintf(w, "%s: ghost_sessions=%d (blocked=%d, active_bindings=%d) dead_custody=%d stale_workers=%d\n", name, c.GhostSessions, c.BlockedGhosts, c.GhostBindings, c.DeadCustody, c.StaleWorkers)
	}
	counts("before", r.Before)
	if r.After != nil {
		counts("after", *r.After)
	}
}

func done(d bool) string {
	if d {
		return " (done)"
	}
	return ""
}
