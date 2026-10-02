package app

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"
	gopevents "github.com/hollis-labs/go-providers/provider/events"

	"github.com/hollis-labs/tether/internal/store"
)

// trackTurnSubmission marks a failure as having happened after the turn ran, and
// the reply dispatcher relies on that to never run a reply twice. Only a
// submission that opened the turn can be marked, and only once the turn produced
// output or finished: a rejection that did nothing, or a failed attempt to steer
// a turn some other submission owns, must stay retryable.
func TestOnlyAnErrorAfterTheTurnRanIsMarkedAsHavingRun(t *testing.T) {
	failure := errors.New("exit status 1")
	cases := []struct {
		name string
		run  func(svc *Service, output *sessionTurnOutput) error
		want bool
	}{
		{"the turn produced output, then the runtime failed", func(svc *Service, output *sessionTurnOutput) error {
			return svc.trackTurnSubmission("s1", func() error {
				output.observeProvider(gopevents.Delta{Text: "working"})
				return failure
			})
		}, true},
		{"the turn finished during the submission, then the runtime failed", func(svc *Service, output *sessionTurnOutput) error {
			return svc.trackTurnSubmission("s1", func() error {
				output.observeProvider(gopevents.Done{Text: "reply"})
				return failure
			})
		}, true},
		{"the runtime rejected the turn before it did anything", func(svc *Service, _ *sessionTurnOutput) error {
			return svc.trackTurnSubmission("s1", func() error { return failure })
		}, false},
		{"steering a turn another submission opened was rejected", func(svc *Service, output *sessionTurnOutput) error {
			if err := svc.trackTurnSubmission("s1", func() error {
				output.observeProvider(gopevents.Delta{Text: "working"})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			return svc.trackTurnSubmission("s1", func() error { return failure })
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, output := outputHarness(t, nil)
			svc.turnOutputs.Store("s1", output)
			err := tc.run(svc, output)
			if !errors.Is(err, failure) {
				t.Fatalf("the runtime's error was lost: %v", err)
			}
			var ran *turnRanError
			if got := errors.As(err, &ran); got != tc.want {
				t.Fatalf("marked as having run = %v, want %v", got, tc.want)
			}
		})
	}
}

// A daemon that stops while a reply is being injected cannot know whether the
// runtime took it. The row must stay 'delivering' for startup recovery, which
// never resends it to a session that is still running. Requeueing it would let
// the next process deliver the same reply a second time.
func TestAShutdownDuringInjectionLeavesTheReplyForRecoveryNotTheQueue(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	entered := make(chan struct{})
	var once sync.Once
	h.onSend = func(string, string) error {
		once.Do(func() { close(entered) })
		<-h.d.ctx.Done() // a subprocess runtime blocks for the whole turn
		return h.d.ctx.Err()
	}
	receipt, err := h.reply(h.routed("s1").ID, "in flight at shutdown", false)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the reply was never injected")
	}
	h.d.stop()
	got := h.state(receipt.ReplyID)
	if got.State != store.RoutingReplyDelivering || got.Attempts != 1 {
		t.Fatalf("reply after shutdown mid-injection = %+v, want delivering after one attempt", got)
	}
	settleQuiet()
	if n := h.rt.sendCallCount("s1"); n != 1 {
		t.Fatalf("send calls = %d, want 1", n)
	}
}

// A failed submission waits out a backoff before the next attempt: a runtime that
// is failing is not hammered, and the delivery view tells a consumer when the
// next attempt is due.
func TestAFailedSubmitIsRetriedAfterABackoffNotAtOnce(t *testing.T) {
	h := newReplyHarness(t)
	replySubmitBackoff = time.Hour // newReplyHarness restores it
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	h.rt.setSendErr("s1", errors.New("stdin closed"))
	receipt, err := h.reply(h.routed("s1").ID, "flaky runtime", false)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.state(receipt.ReplyID).Attempts < 1 || h.state(receipt.ReplyID).State != store.RoutingReplyQueued {
		if time.Now().After(deadline) {
			t.Fatalf("the failed attempt was never recorded: %+v", h.state(receipt.ReplyID))
		}
		time.Sleep(5 * time.Millisecond)
	}
	settleQuiet() // a retry with no backoff would have run by now
	got := h.state(receipt.ReplyID)
	if got.State != store.RoutingReplyQueued || got.Reason != ReplyReasonSubmitFailed || got.Attempts != 1 {
		t.Fatalf("reply = %+v, want queued after one failed attempt", got)
	}
	if got.NextAttemptAt == nil || time.Until(*got.NextAttemptAt) < 30*time.Minute {
		t.Fatalf("next attempt = %v, want it backed off", got.NextAttemptAt)
	}
	if n := h.rt.sendCallCount("s1"); n != 1 {
		t.Fatalf("send calls = %d, want 1 (the retry must wait for its backoff)", n)
	}
}
