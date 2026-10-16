package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/agentevents"
)

// newHookCommand is `tether-worker hook claude`: the command the Claude lifecycle hooks this worker plants run
// (CW-20261014-0143). Same contract as `tether hook claude`: observe-only (silent, exit 0 on every path), fail-open, no
// token, no catalog or network; the work is agentevents.RecordClaudeHook.
func newHookCommand() *cobra.Command {
	var session, spool string
	cmd := &cobra.Command{Use: "hook", Short: "Run by an agent's lifecycle hooks: record the event to the session spool (always exits 0)", Hidden: true}
	cmd.PersistentFlags().StringVar(&session, "session", "", "Tether session id the events belong to")
	cmd.PersistentFlags().StringVar(&spool, "spool", "", "spool directory (absolute path)")
	cmd.AddCommand(&cobra.Command{
		Use: "claude", Short: "Handle a Claude Code hook payload on stdin", Args: cobra.NoArgs, SilenceUsage: true, SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			if class := agentevents.RecordClaudeHook(c.Context(), c.InOrStdin(), session, spool); class != "" {
				fmt.Fprintf(c.ErrOrStderr(), "tether-worker hook: %s\n", class)
			}
			return nil
		},
	})
	return cmd
}
