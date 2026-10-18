package agentevents

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
)

// MaxHookPayload bounds what a hook reads from stdin. A real payload is a few KiB; tool results can be larger but only their
// size is recorded, so reading past this adds nothing.
const MaxHookPayload = 1 << 20

// HookWriteBudget caps the whole record (read, map, lock, append): a hook runs inside the agent's tool loop, so a stuck spool
// lock or a slow disk must cost it at most this, then the hook gives up and exits 0 (architect ruling
// agent_hook_spool_location_and_trust_2026_10_16: ~200 ms).
const HookWriteBudget = 200 * time.Millisecond

// hookStopGrace is how long a hook that ran out of budget waits for its writer to notice and stop.
const hookStopGrace = 25 * time.Millisecond

// RecordClaudeHook returns "" on success or when there is nothing to do, else a short error class (no paths, no content).
func RecordClaudeHook(ctx context.Context, in io.Reader, session, spool string) string {
	if session == "" || spool == "" {
		return "" // not a managed session; nothing to record
	}
	if !filepath.IsAbs(spool) {
		return "spool_not_absolute" // a planted command must not write relative to the agent's cwd
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, HookWriteBudget)
	defer cancel()
	done := make(chan string, 1)
	go func() { done <- recordClaudePayload(ctx, in, session, spool) }()
	var class string
	select {
	case class = <-done:
	case <-ctx.Done():
		class = "timeout"
		// A writer waiting on the lock polls ctx, so it returns within a few ms; wait briefly so it is not left running
		// (and then writing) after this returns. One stuck in a syscall is abandoned: the process exits right after.
		select {
		case c := <-done:
			if c == "" {
				class = "" // it finished at the deadline, so the event was recorded
			}
		case <-time.After(hookStopGrace):
		}
	}
	if class == "timeout" || class == "record_failed" {
		// The event is dropped; count it where the relay and an operator can see it (lock-free, best effort).
		(&Spool{Dir: spool}).NoteHookDrop(session, class)
	}
	return class
}

func recordClaudePayload(ctx context.Context, in io.Reader, session, spool string) string {
	payload, err := io.ReadAll(io.LimitReader(in, MaxHookPayload+1))
	if err != nil {
		return "read_failed"
	}
	if len(payload) > MaxHookPayload {
		return "payload_too_large" // truncated JSON would not parse; say so instead of dropping the event silently
	}
	obs, err := ClaudeHook(payload)
	if err != nil {
		return "payload_invalid"
	}
	if len(obs) == 0 {
		return ""
	}
	rec := &Recorder{
		App:       "tether",
		SessionID: session,
		Sink:      &Spool{Dir: spool},
		Process: runtimeevents.Process{
			PID: os.Getppid(), Provider: "claude", Runtime: "hooks", ProviderSessionID: ClaudeSessionID(payload),
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
