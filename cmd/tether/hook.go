package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/agentevents"
)

// newHookCommand is `tether hook <agent>`: the command an agent CLI runs for its lifecycle hooks (CW-20261014-0143). It reads one
// hook payload on stdin, maps it to catalog events, and appends them to the local spool. It is the interim half of the architect's
// rulings (agent_hooks_one_hook_pulse_and_spool_2026_10_16): spool only, planted behind a default-off flag, next to the owner's
// hand-installed pulse hook.
//
// Properties the review checklist asks for, each held here and tested:
//   - observe-only: nothing on stdout and exit status 0 on every path, so it can never make the agent allow, deny or block;
//   - fail-open: a bad payload, an unreadable spool or an unknown event ends in exit 0 (one line on stderr, which an agent
//     shows only for exit 2), never an error and never a delay beyond reading stdin;
//   - nothing secret: no token in the arguments or environment it reads; the session id and spool directory it is told are not
//     credentials, and event payloads carry names, ids and sizes only (see agentevents.ClaudeHook);
//   - no catalog, no database, no network: it runs once per tool call, so it does the minimum.
func newHookCommand() *cobra.Command {
	var session, spool string
	cmd := &cobra.Command{
		Use:    "hook",
		Short:  "Run by an agent's lifecycle hooks: record the event to the local spool (always exits 0)",
		Hidden: true,
	}
	cmd.PersistentFlags().StringVar(&session, "session", "", "Tether session id the events belong to")
	cmd.PersistentFlags().StringVar(&spool, "spool", "", "spool directory (absolute path)")
	cmd.AddCommand(&cobra.Command{
		Use:           "claude",
		Short:         "Handle a Claude Code hook payload on stdin",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			if class := agentevents.RecordClaudeHook(c.Context(), c.InOrStdin(), session, spool); class != "" {
				// A class only: an error's text can carry a path or part of a payload, and this line may reach a log.
				fmt.Fprintf(c.ErrOrStderr(), "tether hook: %s\n", class)
			}
			return nil // observe-only and fail-open: never a non-zero exit
		},
	})
	return cmd
}
