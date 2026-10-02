package app

// Tests for the independent review of the reply path (CW-20261002-0065, PR #142):
// double delivery on a failed subprocess turn, runtimes with no turn feed,
// recovery and stale rows, ordering of interrupting replies, and the mutations
// the review found surviving.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"
	messaging "github.com/hollis-labs/go-messaging"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

// waitNotRunning waits until no drain is running for the session.
func (h *replyHarness) waitNotRunning(session string) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.d.mu.Lock()
		st, ok := h.d.sessions[session]
		running := ok && st.running
		h.d.mu.Unlock()
		if !running {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("a drain of %s never finished", session)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *replyHarness) queued(session, body string, interrupt bool) store.RoutingReply {
	h.t.Helper()
	r, _, err := h.st.CreateRoutingReply(context.Background(), store.NewRoutingReply{
		From: messaging.Address{Kind: messaging.KindUser, Authority: "local", ID: "chris"}, ParentID: "p", Body: body,
		TargetSessionID: session, LogicalAgentID: "worker", Actor: "msg://user/local/chris", Interrupt: interrupt})
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

// ─── (1) a turn that ran is not run again ───────────────────────────────────

// Below the process layer: an error that comes back before the runtime took the
// turn is retried; one that comes back after is reported, never repeated.
func TestAnErrorAfterTheTurnWasTakenIsReportedNotRetried(t *testing.T) {
	for name, tc := range map[string]struct {
		err       error
		wantState store.RoutingReplyState
		wantCalls int
	}{
		"the turn ran":                 {&turnRanError{errors.New("runner: process exited 1")}, store.RoutingReplyDelivered, 1},
		"rejected before it was taken": {errors.New("stdin closed"), store.RoutingReplyUndeliverable, replyMaxAttempts},
		"a wrapped turn failure":       {fmt.Errorf("session: %w", &turnRanError{errors.New("exit 2")}), store.RoutingReplyDelivered, 1},
	} {
		t.Run(name, func(t *testing.T) {
			h := newReplyHarness(t)
			h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
			h.rt.setSendErr("s1", tc.err)
			receipt, _ := h.reply(h.routed("s1").ID, "do it", false)
			got := h.waitState(receipt.ReplyID, tc.wantState)
			settleQuiet()
			if n := h.rt.sendCallCount("s1"); n != tc.wantCalls {
				t.Fatalf("injected %d times, want %d (%+v)", n, tc.wantCalls, got)
			}
			if tc.wantState == store.RoutingReplyDelivered && (got.Reason != ReplyReasonTurnFailed || got.DeliveredToSessionID != "s1" || got.Attempts != 1) {
				t.Fatalf("a failed turn is delivered with its reason: %+v", got)
			}
		})
	}
}

// ─── (2) a runtime with no turn feed ────────────────────────────────────────

func TestAReplyForARuntimeWithNoTurnFeedIsRefusedAtAccept(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("pty", true, agentsessions.LiveStateIdle)
	h.noFeed["pty"] = true
	_, err := h.reply(h.routed("pty").ID, "hello?", false)
	if !errors.Is(err, api.ErrReplyNoTurnFeed) {
		t.Fatalf("got %v, want the typed no-turn-feed refusal", err)
	}
	if rows, _ := h.st.RoutingRepliesInState(context.Background(), store.RoutingReplyQueued, 10); len(rows) != 0 {
		t.Fatalf("a refused reply must not queue: %+v", rows)
	}
	if h.rt.sendCallCount("pty") != 0 {
		t.Fatal("injected into a PTY")
	}
	// An ended session is still accepted: its replies hand off or settle, and the
	// successor is checked when the reply gets there.
	if _, err := h.reply(h.routed("gone").ID, "later", false); err != nil {
		t.Fatalf("ended session: %v", err)
	}
}

// A reply that reaches a no-feed session later (a hand-off target) is settled with
// a reason, never left to wait for a boundary that cannot come.
func TestRepliesQueuedForANoFeedSessionAreSettledNotStranded(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("pty", true, agentsessions.LiveStateIdle)
	h.noFeed["pty"] = true
	a, b := h.queued("pty", "one", false), h.queued("pty", "two", false)
	h.d.notify("pty")
	for _, id := range []string{a.ReplyID, b.ReplyID} {
		got := h.waitState(id, store.RoutingReplyUndeliverable)
		if got.Reason != ReplyReasonNoTurnFeed || got.Detail == "" {
			t.Fatalf("got %+v", got)
		}
	}
	if h.rt.sendCallCount("pty") != 0 {
		t.Fatal("injected into a PTY")
	}
}

// ─── (3) start, recovery and stale rows ─────────────────────────────────────

func TestTheReplyPathIsNotWiredWhenRecoveryFails(t *testing.T) {
	st, reg := newWakeHarness(t)
	svc := &Service{Store: st, Registry: reg}
	if err := st.Close(); err != nil { // every query now fails
		t.Fatal(err)
	}
	err := svc.StartRoutingReplies(context.Background())
	if err == nil {
		t.Fatal("recovery failed and StartRoutingReplies said nothing")
	}
	if svc.RoutingReplyWired() {
		t.Fatal("RoutingReplyWired is true although recovery failed")
	}
	if _, err := svc.SubmitRoutingReply(context.Background(), api.RoutingReplyRequest{ParentID: "x", Body: "y",
		Caller: identity.Principal{ID: "msg://user/local/c"}}); !errors.Is(err, api.ErrRoutingRepliesNotWired) {
		t.Fatalf("a reply was accepted by an unwired path: %v", err)
	}
}

func TestStaleDeliveringRowsAreResolvedBySweepNotOnlyAtStartup(t *testing.T) {
	ctx := context.Background()
	t.Run("session still running: not sent again", func(t *testing.T) {
		h := newReplyHarness(t)
		h.rt.setAlive("s1", true, agentsessions.LiveStateProcessing)
		r := h.queued("s1", "maybe sent", false)
		if ok, _ := h.st.ClaimRoutingReply(ctx, r.ReplyID); !ok { // left 'delivering' by a drain that is gone
			t.Fatal("claim")
		}
		if _, err := h.d.sweep(ctx); err != nil {
			t.Fatal(err)
		}
		got := h.waitState(r.ReplyID, store.RoutingReplyUndeliverable)
		if got.Reason != ReplyReasonRestart || h.rt.sendCallCount("s1") != 0 {
			t.Fatalf("got %+v sends=%d", got, h.rt.sendCallCount("s1"))
		}
	})
	t.Run("session gone: back to the queue, then the ended-session path", func(t *testing.T) {
		h := newReplyHarness(t)
		r := h.queued("s-gone", "carry on", false)
		if ok, _ := h.st.ClaimRoutingReply(ctx, r.ReplyID); !ok {
			t.Fatal("claim")
		}
		if _, err := h.d.sweep(ctx); err != nil {
			t.Fatal(err)
		}
		got := h.waitState(r.ReplyID, store.RoutingReplyUndeliverable)
		if got.Reason != ReplyReasonNoBinding {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a reply this process is injecting is never touched", func(t *testing.T) {
		h := newReplyHarness(t)
		h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
		release := make(chan struct{})
		entered := make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		// A failed assertion below must not leave sendTurn blocked: the harness
		// cleanup joins the dispatcher, which would wait on it forever.
		t.Cleanup(unblock)
		h.onSend = func(string, string) error { close(entered); <-release; return nil }
		receipt, _ := h.reply(h.routed("s1").ID, "slow turn", false)
		select {
		case <-entered: // sendTurn is blocked: a blocking runtime mid-turn
		case <-time.After(5 * time.Second):
			t.Fatal("the reply was never injected")
		}
		for i := 0; i < 3; i++ {
			if _, err := h.d.sweep(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if got := h.state(receipt.ReplyID); got.State != store.RoutingReplyDelivering {
			t.Fatalf("a live injection was resolved from under itself: %+v", got)
		}
		unblock()
		h.waitState(receipt.ReplyID, store.RoutingReplyDelivered)
		if n := h.rt.sendCallCount("s1"); n != 1 {
			t.Fatalf("injected %d times", n)
		}
	})
}

func TestStalePendingReservationsAreSettledButAHeldOneIsNot(t *testing.T) {
	h := newReplyHarness(t)
	ctx := context.Background()
	pending := func(session string) store.RoutingReply {
		r, _, err := h.st.CreateRoutingReply(ctx, store.NewRoutingReply{
			From: messaging.Address{Kind: messaging.KindUser, Authority: "local", ID: "chris"}, ParentID: "p", Body: "interrupt",
			TargetSessionID: session, Actor: "msg://user/local/chris", Interrupt: true, Pending: true})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	crashed, held := pending("s-crashed"), pending("s-held")
	release := h.d.lockSubmit("s-held") // an interrupting submission for s-held is mid-cancel
	if _, err := h.d.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	got := h.waitState(crashed.ReplyID, store.RoutingReplyUndeliverable)
	if got.Reason != ReplyReasonInterruptUnconfirmed {
		t.Fatalf("crashed reservation: %+v", got)
	}
	if got := h.state(held.ReplyID); got.State != store.RoutingReplyPending {
		t.Fatalf("a reservation that a live submission holds was settled: %+v", got)
	}
	release()
}

// ─── (4) an interrupting reply is the next turn ─────────────────────────────

func TestAnInterruptingReplyIsTheNextTurnAheadOfOlderQueuedReplies(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	h.onSend = func(id, _ string) error { h.setBusy(id, true); return nil } // an async runtime: the turn starts
	h.setBusy("s1", true)                                                   // a turn is running
	parent := h.routed("s1")
	older, _ := h.reply(parent.ID, "older, waiting", false)
	h.intr.onCancel = func(id string) {
		// The cancel ends the turn: its boundary notifies the dispatcher, exactly as
		// settleTurn does. Nothing queued may be handed the turn in this gap.
		h.setBusy(id, false)
		h.d.notify(id)
		h.waitNotRunning(id)
	}
	interrupting, err := h.reply(parent.ID, "interrupting", true)
	if err != nil {
		t.Fatal(err)
	}
	h.waitState(interrupting.ReplyID, store.RoutingReplyDelivered)
	if got := h.state(older.ReplyID); got.State != store.RoutingReplyQueued {
		t.Fatalf("the older reply took the turn the cancel freed: %+v", got)
	}
	h.setBusy("s1", false) // the interrupting reply's turn ends
	h.d.notify("s1")
	h.waitState(older.ReplyID, store.RoutingReplyDelivered)
	if got := h.rt.sendCalls; len(got) != 2 || got[0].Text != "interrupting" || got[1].Text != "older, waiting" {
		t.Fatalf("turns = %+v, want the interrupting reply first", got)
	}
}

func TestARefusedInterruptLeavesNoReplyAndFreesTheKey(t *testing.T) {
	h := newReplyHarness(t)
	ctx := context.Background()
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	parent := h.routed("s1")
	h.intr.err = &TurnInterruptRefusal{Reason: TurnInterruptUnsupported, SessionID: "s1"}
	req := api.RoutingReplyRequest{ParentID: parent.ID, Body: "stop", Interrupt: true, Verified: true, IdempotencyKey: "k",
		Caller: identity.Principal{ID: "msg://user/local/chris"}}
	if _, err := h.svc.SubmitRoutingReply(ctx, req); !errors.Is(err, api.ErrReplyInterruptUnsupported) {
		t.Fatalf("got %v", err)
	}
	var replies, messages int
	if err := h.st.DB().QueryRow(`SELECT count(*) FROM routing_replies`).Scan(&replies); err != nil || replies != 0 {
		t.Fatalf("reservation left behind: %d %v", replies, err)
	}
	if err := h.st.DB().QueryRow(`SELECT count(*) FROM messages WHERE in_reply_to=?`, parent.ID).Scan(&messages); err != nil || messages != 0 {
		t.Fatalf("message left behind: %d %v", messages, err)
	}
	// The key is free: the same request succeeds once the runtime can cancel.
	h.intr.err = nil
	if receipt, err := h.svc.SubmitRoutingReply(ctx, req); err != nil || receipt.Duplicate {
		t.Fatalf("retry: %+v %v", receipt, err)
	}
}

// The client disconnects while the cancel is in flight (or just after it). The
// cancel happened; the reply must exist, and a retry must find it, not cancel again.
func TestAnInterruptingReplyIsNotLostWhenTheClientDisconnectsAfterTheCancel(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("s1", true, agentsessions.LiveStateProcessing)
	parent := h.routed("s1")
	ctx, disconnect := context.WithCancel(context.Background())
	h.intr.onCancel = func(string) { disconnect() }
	req := api.RoutingReplyRequest{ParentID: parent.ID, Body: "stop", Interrupt: true, Verified: true, IdempotencyKey: "k",
		Caller: identity.Principal{ID: "msg://user/local/chris"}}
	first, err := h.svc.SubmitRoutingReply(ctx, req)
	if err != nil {
		t.Fatalf("the cancel succeeded and the reply must be accepted: %v", err)
	}
	if got := h.state(first.ReplyID); got.State != store.RoutingReplyQueued {
		t.Fatalf("reply = %+v", got)
	}
	retry, err := h.svc.SubmitRoutingReply(context.Background(), req)
	if err != nil || !retry.Duplicate || retry.ReplyID != first.ReplyID {
		t.Fatalf("retry %+v %v", retry, err)
	}
	if n := h.intr.callCount(); n != 1 {
		t.Fatalf("the retry canceled again (%d cancels)", n)
	}
}

func TestInterruptingSubmissionsToOneSessionDoNotOverlap(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("s1", true, agentsessions.LiveStateProcessing)
	h.intr.delay = 40 * time.Millisecond
	parent := h.routed("s1")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.reply(parent.ID, fmt.Sprintf("interrupt %d", i), true); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	h.intr.mu.Lock()
	defer h.intr.mu.Unlock()
	if h.intr.maxActive != 1 || len(h.intr.calls) != 4 {
		t.Fatalf("cancels overlapped: max concurrent %d of %d", h.intr.maxActive, len(h.intr.calls))
	}
}

// ─── ordering ───────────────────────────────────────────────────────────────

func TestAYoungerDueReplyDoesNotJumpAnOlderOneInBackoff(t *testing.T) {
	h := newReplyHarness(t)
	ctx := context.Background()
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	older, younger := h.queued("s1", "older", false), h.queued("s1", "younger", false)
	if ok, _ := h.st.ClaimRoutingReply(ctx, older.ReplyID); !ok {
		t.Fatal("claim")
	}
	h.d.requeue(older, store.RoutingReplyRequeue{Reason: ReplyReasonSubmitFailed, NotBefore: time.Now().Add(250 * time.Millisecond)})
	h.d.notify("s1")
	settleQuiet()
	if h.rt.sendCallCount("s1") != 0 {
		t.Fatalf("the younger reply jumped the older one: %+v", h.rt.sendCalls)
	}
	h.waitState(older.ReplyID, store.RoutingReplyDelivered) // its retry timer fires
	if got := h.state(younger.ReplyID); got.State != store.RoutingReplyQueued {
		t.Fatalf("the younger reply was delivered at the older one's boundary: %+v", got)
	}
}

// ─── what leaves the daemon ─────────────────────────────────────────────────

func TestDetailIsBoundedOnOneLineInTheViewAndInTheEvent(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	noisy := "stdin closed\nstderr (last 2 KB):\n" + strings.Repeat("TOKEN=hunter2 \n", 200)
	h.rt.setSendErr("s1", errors.New(noisy))
	receipt, _ := h.reply(h.routed("s1").ID, "x", false)
	got := h.waitState(receipt.ReplyID, store.RoutingReplyUndeliverable)
	view, err := h.svc.RoutingReplyDelivery(context.Background(), receipt.ReplyID)
	if err != nil {
		t.Fatal(err)
	}
	_, evs := h.waitEvents(1)
	for name, detail := range map[string]string{"row": got.Detail, "view": view.Detail, "event": evs[0].Detail} {
		// 256 is the documented bound (docs/api/README.md): not the constant under test.
		if len(detail) == 0 || len(detail) > 256 || strings.ContainsAny(detail, "\n\r") {
			t.Fatalf("%s detail = %q (%d bytes)", name, detail, len(detail))
		}
	}
}

func TestEventsCarryNoReplyTextAndNameTheActor(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	const secret = "the-launch-code-is-1234"
	delivered, _ := h.reply(h.routed("s1").ID, secret, false)
	h.waitState(delivered.ReplyID, store.RoutingReplyDelivered)
	h.rt.setAlive("s-gone", false, agentsessions.LiveStateIdle)
	lost, _ := h.reply(h.routed("s-gone").ID, secret, false)
	h.waitState(lost.ReplyID, store.RoutingReplyUndeliverable)
	_, evs := h.waitEvents(2)
	for _, ev := range evs {
		data, _ := json.Marshal(ev)
		if strings.Contains(string(data), secret) {
			t.Fatalf("an event carries the reply text: %s", data)
		}
		if ev.Actor != "msg://user/local/chris" {
			t.Fatalf("event actor = %q", ev.Actor)
		}
	}
	if got := h.state(delivered.ReplyID); got.Actor != "msg://user/local/chris" {
		t.Fatalf("stored actor = %q", got.Actor)
	}
}

// ─── lifecycle ──────────────────────────────────────────────────────────────

func TestStopWaitsForADrainAndAStoppedDispatcherTakesNoKicks(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	release := make(chan struct{})
	entered := make(chan struct{})
	h.onSend = func(string, string) error { close(entered); <-release; return nil } // ignores the context, like a stuck runtime
	h.reply(h.routed("s1").ID, "slow", false)                                       //nolint:errcheck // delivery is what is under test
	<-entered
	stopped := make(chan struct{})
	go func() { h.d.stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop returned while a drain was still running")
	case <-time.After(80 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop never returned")
	}
	h.d.notify("s2")
	h.d.notifyNew("s3")
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	for id, st := range h.d.sessions {
		t.Fatalf("a stopped dispatcher took a kick for %s: %+v", id, st)
	}
}

// The session exits with no turn open (so settleTurn has nothing to settle): the
// exit itself must still resolve replies that were waiting on it.
func TestAnExitWithNoOpenTurnStillResolvesQueuedReplies(t *testing.T) {
	svc, output, rt, _ := replyOverRealTurnState(t)
	r := queueReplyFor(t, svc, "s1", "waiting on an idle session")
	rt.setAlive("s1", false, agentsessions.LiveStateIdle)
	output.flush() // the launch goroutine's exit hook: the only notification
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := svc.Store.RoutingReply(context.Background(), r.ReplyID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State == store.RoutingReplyUndeliverable {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the exit did not resolve the reply: %+v", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ─── what counts as routed ──────────────────────────────────────────────────

func TestOnlyAChannelPublicationIsRoutedNotAMailboxMessageFromASession(t *testing.T) {
	h := newReplyHarness(t)
	mailbox, err := h.st.MessagingStore().Send(context.Background(), messaging.Envelope{Kind: messaging.MsgKindRequest,
		From: messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "s1"},
		To:   messaging.Address{Kind: messaging.KindAgent, Authority: "local", ID: "boss"}, Payload: []byte(`"ping"`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.reply(mailbox.ID, "x", false); !errors.Is(err, api.ErrReplyTargetNotSession) {
		t.Fatalf("a mailbox message from a session was queued for its sender: %v", err)
	}
	if rows, _ := h.st.RoutingRepliesInState(context.Background(), store.RoutingReplyQueued, 10); len(rows) != 0 {
		t.Fatalf("queued: %+v", rows)
	}
}

var _ = events.KindRoutingReplyDelivered
