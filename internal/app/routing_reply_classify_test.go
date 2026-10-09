package app

import (
	"errors"
	"fmt"
	"testing"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/substrate/harness/adapters/provider"
	"github.com/hollis-labs/substrate/harness/runner"

	"github.com/hollis-labs/tether/internal/store"
)

// ─── what counts as "the turn ran" (CW-20261002-0065 review F1, F4) ─────────

// A CLI that was launched and then refused the turn (no login, a dead resume id)
// still opens the turn marker, so a failure behind an open marker is not proof
// that the model saw the reply. The runtime's own "did not run" signals outrank the
// marker: each is retried, because a retry can succeed and nothing has acted on the
// reply. The last case injects a bare process-exit error WITHOUT the turn feed, so
// nothing marks it as having run; that is not what production does (the adapter's
// synthesized terminal makes a bare exit turn_failed, once: see
// TestAFailedSubprocessTurnIsNotRetried), it only pins that the dispatcher itself
// does not treat an ExitError as proof.
func TestAFailureTheRuntimeSaysDidNotRunTheTurnIsRetriedWhateverTheMarkerSays(t *testing.T) {
	exit := func() error { return &runner.ExitError{Code: 1} }
	for name, err := range map[string]error{
		"a lost resume session behind an open marker":                            &turnRanError{&agentsessions.SessionLostError{RequestedID: "gone", Err: exit()}},
		"no login behind an open marker":                                         &turnRanError{fmt.Errorf("agentsessions: %w: %w", provider.ErrProviderNotAuthenticated, exit())},
		"a process that would not start behind a marker":                         &turnRanError{&runner.StartError{Err: errors.New("fork/exec: no such file")}},
		"a sandbox that could not be set up behind marker":                       &turnRanError{&runner.SandboxError{Err: errors.New("sandbox denied")}},
		"a process exit error that reached the dispatcher without the turn feed": exit(),
	} {
		t.Run(name, func(t *testing.T) {
			h := newReplyHarness(t)
			h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
			h.rt.setSendErr("s1", err)
			receipt, _ := h.reply(h.routed("s1").ID, "do it", false)
			got := h.waitState(receipt.ReplyID, store.RoutingReplyUndeliverable)
			settleQuiet()
			if got.Reason != ReplyReasonSubmitFailed || got.Attempts != replyMaxAttempts || h.rt.sendCallCount("s1") != replyMaxAttempts {
				t.Fatalf("a failure that did not run the turn was not retried: %+v after %d sends", got, h.rt.sendCallCount("s1"))
			}
		})
	}
}

// A rejected submission can still have had an in-flight turn's first event bind
// the provisional marker, so the error arrives wrapped as "the turn ran". It is
// still a rejection: the reply waits for the turn to end and is then delivered.
func TestARejectedSubmissionIsAWaitEvenWhenItsMarkerLooksLikeTheTurnRan(t *testing.T) {
	for name, cause := range map[string]error{
		"a turn already in flight": fmt.Errorf("%w: busy", agentsessions.ErrTurnInFlight),
		"a session that went away": fmt.Errorf("%w: gone", agentsessions.ErrSessionNotRunning),
	} {
		t.Run(name, func(t *testing.T) {
			h := newReplyHarness(t)
			h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
			rejections := 2
			h.onSend = func(string, string) error {
				if rejections > 0 {
					rejections--
					return &turnRanError{cause}
				}
				return nil
			}
			receipt, _ := h.reply(h.routed("s1").ID, "wait for me", false)
			got := h.waitState(receipt.ReplyID, store.RoutingReplyDelivered)
			if got.Reason == ReplyReasonTurnFailed || got.Attempts != 1 || h.rt.sendCallCount("s1") != 3 {
				t.Fatalf("a rejection was taken for a turn that ran: %+v after %d sends", got, h.rt.sendCallCount("s1"))
			}
		})
	}
}
