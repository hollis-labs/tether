package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
			if class := recordClaudeHook(c.Context(), c.InOrStdin(), session, spool); class != "" {
				// A class only: an error's text can carry a path or part of a payload, and this line may reach a log.
				fmt.Fprintf(c.ErrOrStderr(), "tether hook: %s\n", class)
			}
			return nil // observe-only and fail-open: never a non-zero exit
		},
	})
	return cmd
}

// hookWriteBudget caps the whole record (read, map, lock, append): a hook runs inside the agent's tool loop, so a stuck spool
// lock or a slow disk must cost it at most this, then the hook gives up and exits 0 (architect ruling
// agent_hook_spool_location_and_trust_2026_10_16: ~200 ms).
const hookWriteBudget = 200 * time.Millisecond

// recordClaudeHook returns "" on success or when there is nothing to do, else a short error class (no paths, no content).
func recordClaudeHook(ctx context.Context, in io.Reader, session, spool string) string {
	if session == "" || spool == "" {
		return "" // not a managed session; nothing to record
	}
	if !filepath.IsAbs(spool) {
		return "spool_not_absolute" // a planted command must not write relative to the agent's cwd
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, hookWriteBudget)
	defer cancel()
	done := make(chan string, 1)
	go func() { done <- recordClaudePayload(ctx, in, session, spool) }()
	var class string
	select {
	case class = <-done:
	case <-ctx.Done():
		class = "timeout" // the process exits right after; an append still in flight is abandoned
	}
	if class == "timeout" || class == "record_failed" {
		// The event is dropped; count it where the relay and an operator can see it (lock-free, best effort).
		(&agentevents.Spool{Dir: spool}).NoteHookDrop(session, class)
	}
	return class
}

func recordClaudePayload(ctx context.Context, in io.Reader, session, spool string) string {
	payload, err := io.ReadAll(io.LimitReader(in, maxHookPayload+1))
	if err != nil {
		return "read_failed"
	}
	if len(payload) > maxHookPayload {
		return "payload_too_large" // truncated JSON would not parse; say so instead of dropping the event silently
	}
	obs, err := agentevents.ClaudeHook(payload)
	if err != nil {
		return "payload_invalid"
	}
	if len(obs) == 0 {
		return ""
	}
	rec := &agentevents.Recorder{
		App:       "tether",
		SessionID: session,
		Sink:      &agentevents.Spool{Dir: spool},
		Process: runtimeevents.Process{
			PID: os.Getppid(), Provider: "claude", Runtime: "hooks", ProviderSessionID: agentevents.ClaudeSessionID(payload),
		},
	}
	class := ""
	for _, o := range obs {
		if _, err := rec.Record(ctx, o); err != nil && class == "" {
			class = "record_failed"
		}
	}
	return class
}
