package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/agentevents"
)

// maxHookPayload bounds what a hook reads from stdin. A real payload is a few KiB; tool results can be larger but only their
// size is recorded, so reading past this adds nothing.
const maxHookPayload = 1 << 20

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
			if err := recordClaudeHook(c.Context(), c.InOrStdin(), session, spool); err != nil {
				fmt.Fprintf(c.ErrOrStderr(), "tether hook: %v\n", err)
			}
			return nil // observe-only and fail-open: never a non-zero exit
		},
	})
	return cmd
}

func recordClaudeHook(ctx context.Context, in io.Reader, session, spool string) error {
	if session == "" || spool == "" {
		return nil // not a managed session; nothing to record
	}
	payload, err := io.ReadAll(io.LimitReader(in, maxHookPayload))
	if err != nil {
		return fmt.Errorf("read payload: %w", err)
	}
	obs, err := agentevents.ClaudeHook(payload)
	if err != nil {
		return err
	}
	if len(obs) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rec := &agentevents.Recorder{
		App:       "tether",
		SessionID: session,
		Sink:      &agentevents.Spool{Dir: spool},
		Process: runtimeevents.Process{
			PID: os.Getppid(), Provider: "claude", Runtime: "hooks", ProviderSessionID: agentevents.ClaudeSessionID(payload),
		},
	}
	var firstErr error
	for _, o := range obs {
		if _, err := rec.Record(ctx, o); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
