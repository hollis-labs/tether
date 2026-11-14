package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"os"

	"github.com/hollis-labs/tether/internal/agentevents"
	"github.com/hollis-labs/tether/internal/workerruntime"
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
	var hubURL, tokenFile, state string
	gate := &cobra.Command{
		Use: "stop-gate", Short: "Claude Stop hook: block the turn once while a typed message waits on this seat's reply (fails open)", Args: cobra.NoArgs, SilenceUsage: true, SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) (err error) {
			// Fail open whatever happens: a hook that panics or errors must never block or fail the agent's turn.
			defer func() {
				if p := recover(); p != nil {
					fmt.Fprintf(c.ErrOrStderr(), "tether-worker stop-gate: internal error (%v); not gating\n", p)
				}
				err = nil
			}()
			workerruntime.RunStopGate(c.Context(), c.InOrStdin(), c.OutOrStdout(), c.ErrOrStderr(), hubURL, tokenFile, os.Getenv("TETHER_AGENT_URN"), state)
			return nil
		},
	}
	gate.Flags().StringVar(&hubURL, "url", "", "the hub's /mcp endpoint")
	gate.Flags().StringVar(&tokenFile, "token-file", "", "the session's agent token file (a path; the token is never an argument)")
	gate.Flags().StringVar(&state, "state", "", "file of message ids that already blocked a stop (each blocks at most once)")
	cmd.AddCommand(gate)
	return cmd
}
