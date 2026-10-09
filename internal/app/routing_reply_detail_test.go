package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/substrate/harness/runner"

	"github.com/hollis-labs/tether/internal/store"
)

// ─── what a consumer is told of a failure (F2) ──────────────────────────────

// A CLI can echo the reply it was given on its stderr, and the runtime layer adds
// that tail to the error text. The row, the delivery view and the event are all
// readable more widely than the reply, so none of them may carry it: a process
// failure is reported as its exit code or signal and nothing else.
func TestAProcessFailureNeverPutsItsOutputInTheRowTheViewOrTheEvent(t *testing.T) {
	const secret = "the reply text the CLI echoed on stderr"
	echoed := func(exit *runner.ExitError) error {
		return fmt.Errorf("runner: process exited %d stderr (last 60 bytes): prompt was: %s: %w", exit.Code, secret, exit)
	}
	cases := map[string]struct {
		err        error
		wantState  store.RoutingReplyState
		wantDetail string
	}{
		"after the turn ran": {&turnRanError{echoed(&runner.ExitError{Code: 3})}, store.RoutingReplyDelivered,
			"the runtime reported a failure after the reply was submitted; it is not retried: the runtime process exited with code 3"},
		"after a signal ended it": {&turnRanError{echoed(&runner.ExitError{Code: -1, Signal: 9})}, store.RoutingReplyDelivered,
			"the runtime reported a failure after the reply was submitted; it is not retried: the runtime process was terminated by signal 9"},
		"after the runner killed it": {&turnRanError{echoed(&runner.ExitError{Code: -1, Signal: 9, Killed: true, Cause: runner.CauseIdleTimeout})}, store.RoutingReplyDelivered,
			"the runtime reported a failure after the reply was submitted; it is not retried: the runtime process was terminated (cause idle_timeout)"},
		"when the turn never ran": {echoed(&runner.ExitError{Code: 7}), store.RoutingReplyUndeliverable,
			"the runtime process exited with code 7"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newReplyHarness(t)
			h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
			h.rt.setSendErr("s1", tc.err)
			receipt, _ := h.reply(h.routed("s1").ID, secret, false)
			row := h.waitState(receipt.ReplyID, tc.wantState)
			view, err := h.svc.RoutingReplyDelivery(context.Background(), receipt.ReplyID)
			if err != nil {
				t.Fatal(err)
			}
			_, evs := h.waitEvents(1)
			if row.Detail != tc.wantDetail || view.Detail != tc.wantDetail || evs[0].Detail != tc.wantDetail {
				t.Fatalf("detail: row %q view %q event %q, want %q", row.Detail, view.Detail, evs[0].Detail, tc.wantDetail)
			}
			if all := fmt.Sprintf("%+v %+v %+v", row, view, evs); strings.Contains(all, secret) {
				t.Fatalf("the process output reached a consumer: %s", all)
			}
		})
	}
}
