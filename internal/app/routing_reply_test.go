package app

// Reply-to-sender (CW-20261002-0065): the dispatcher runs against a REAL
// *store.Store and a REAL *registry.Service; only the process layer is faked
// (fakeRuntime from wake_test.go plus an in-flight turn marker). No real model
// CLI is started.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"
	messaging "github.com/hollis-labs/go-messaging"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

type fakeInterrupter struct {
	mu    sync.Mutex
	calls []string
	err   error
	// onCancel runs inside CancelTurnAndWait, like the turn ending.
	onCancel func(sessionID string)
}

func (f *fakeInterrupter) CancelTurnAndWait(_ context.Context, sessionID, actor string) (TurnInterruptResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, sessionID+"|"+actor)
	err, onCancel := f.err, f.onCancel
	f.mu.Unlock()
	if err == nil && onCancel != nil {
		onCancel(sessionID)
	}
	return TurnInterruptResult{TurnID: "turn-1", OutputTurnID: "out-1"}, err
}

func (f *fakeInterrupter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type replyHarness struct {
	t      *testing.T
	st     *store.Store
	reg    *registry.Service
	rt     *fakeRuntime
	svc    *Service
	d      *replyDispatcher
	intr   *fakeInterrupter
	mu     sync.Mutex
	busy   map[string]bool
	events []events.RoutingReplyEvent
	kinds  []string
	// onSend runs inside sendTurn, after it is recorded.
	onSend func(sessionID, text string) error
}

func newReplyHarness(t *testing.T) *replyHarness {
	t.Helper()
	oldSubmit, oldIdle := replySubmitBackoff, replyIdleBackoff
	replySubmitBackoff, replyIdleBackoff = 5*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { replySubmitBackoff, replyIdleBackoff = oldSubmit, oldIdle })

	st, reg := newWakeHarness(t)
	h := &replyHarness{t: t, st: st, reg: reg, rt: newFakeRuntime(), busy: map[string]bool{}, intr: &fakeInterrupter{}}
	seam := h.rt.seam()
	runtime := replyRuntime{wakeRuntime: wakeRuntime{health: seam.health, sendTurn: func(ctx context.Context, id, text string) error {
		if err := seam.sendTurn(ctx, id, text); err != nil {
			return err
		}
		if h.onSend != nil {
			return h.onSend(id, text)
		}
		return nil
	}}, turnBusy: func(id string) bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.busy[id]
	}}
	h.d = newReplyDispatcher(context.Background(), st, reg, runtime, func(kind string, ev events.RoutingReplyEvent) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.kinds = append(h.kinds, kind)
		h.events = append(h.events, ev)
	})
	t.Cleanup(h.d.stop)
	h.svc = &Service{Store: st, Registry: reg, interrupter: h.intr}
	h.svc.replies.Store(h.d)
	return h
}

func (h *replyHarness) setBusy(session string, busy bool) {
	h.mu.Lock()
	h.busy[session] = busy
	h.mu.Unlock()
}

// waitEvents returns the published events once at least n have arrived: a reply
// is settled in the store before its event is published.
func (h *replyHarness) waitEvents(n int) ([]string, []events.RoutingReplyEvent) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		kinds, evs := append([]string(nil), h.kinds...), append([]events.RoutingReplyEvent(nil), h.events...)
		h.mu.Unlock()
		if len(evs) >= n {
			return kinds, evs
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("saw %d events, want %d", len(evs), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// routed publishes a message a session sent to a channel: the parent of a reply.
func (h *replyHarness) routed(session string) messaging.Envelope {
	h.t.Helper()
	to, err := channels.ChannelAddress("ops")
	if err != nil {
		h.t.Fatal(err)
	}
	env, err := h.st.MessagingStore().Send(context.Background(), messaging.Envelope{
		Kind: messaging.MsgKindNotice, From: messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: session},
		To: to, Payload: []byte(`{"text":"which option?"}`), ContentType: "application/json",
		Metadata: map[string]string{"session_id": session, "logical_agent_id": "worker"},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return env
}

func (h *replyHarness) reply(parent string, body string, interrupt bool) (api.RoutingReplyReceipt, error) {
	return h.svc.SubmitRoutingReply(context.Background(), api.RoutingReplyRequest{
		ParentID: parent, Body: body, Interrupt: interrupt, Verified: true,
		Caller: identity.Principal{ID: "msg://user/local/chris", Kind: "user"},
	})
}

func (h *replyHarness) state(replyID string) store.RoutingReply {
	h.t.Helper()
	r, err := h.st.RoutingReply(context.Background(), replyID)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func (h *replyHarness) waitState(replyID string, want store.RoutingReplyState) store.RoutingReply {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r := h.state(replyID)
		if r.State == want {
			return r
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("reply %s is %q (reason %q detail %q), want %q", replyID, r.State, r.Reason, r.Detail, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// settleQuiet gives a drain that should do nothing time to prove it did nothing.
func settleQuiet() { time.Sleep(60 * time.Millisecond) }

func TestReplyIsInjectedAtTheIdleBoundaryNotOnAcceptance(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	h.setBusy("s1", true) // a turn is in flight
	parent := h.routed("s1")

	receipt, err := h.reply(parent.ID, "pick the second option", false)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.State != "queued" || receipt.TargetSessionID != "s1" || receipt.Duplicate || receipt.Interrupt != "" {
		t.Fatalf("receipt = %+v", receipt)
	}
	settleQuiet()
	if n := h.rt.sendCallCount("s1"); n != 0 {
		t.Fatalf("sent %d turns into a busy session", n)
	}

	// The turn completes: the settle hook notifies, and the reply is the next turn.
	h.setBusy("s1", false)
	h.d.notify("s1")
	got := h.waitState(receipt.ReplyID, store.RoutingReplyDelivered)
	if got.DeliveredToSessionID != "s1" || got.Attempts != 1 || got.Reason != "" {
		t.Fatalf("delivered = %+v", got)
	}
	if n := h.rt.sendCallCount("s1"); n != 1 || h.rt.sendCalls[0].Text != "pick the second option" {
		t.Fatalf("sendCalls = %+v", h.rt.sendCalls)
	}
	kinds, evs := h.waitEvents(1)
	if kinds[0] != events.KindRoutingReplyDelivered || evs[0].ReplyID != receipt.ReplyID || evs[0].DeliveredToSessionID != "s1" {
		t.Fatalf("events = %v %+v", kinds, evs)
	}
}

func TestReplyToAnIdleSessionIsInjectedAtOnce(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	receipt, err := h.reply(h.routed("s1").ID, "go", false)
	if err != nil {
		t.Fatal(err)
	}
	h.waitState(receipt.ReplyID, store.RoutingReplyDelivered)
}

func TestReplyWaitsWhileTheRuntimeReportsProcessing(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("s1", true, agentsessions.LiveStateProcessing) // e.g. a PTY with no turn marker
	receipt, _ := h.reply(h.routed("s1").ID, "later", false)
	settleQuiet()
	if h.rt.sendCallCount("s1") != 0 {
		t.Fatal("delivered into a processing session")
	}
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	if n, err := h.d.sweep(context.Background()); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	h.waitState(receipt.ReplyID, store.RoutingReplyDelivered)
}

func TestOneReplyPerIdleBoundaryInArrivalOrder(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	h.onSend = func(id, _ string) error { h.setBusy(id, true); return nil } // an async runtime: the turn starts
	h.setBusy("s1", true)
	parent := h.routed("s1")
	var ids []string
	for _, body := range []string{"first", "second", "third"} {
		r, err := h.reply(parent.ID, body, false)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ReplyID)
	}
	for i, id := range ids {
		h.setBusy("s1", false)
		h.d.notify("s1")
		h.waitState(id, store.RoutingReplyDelivered)
		settleQuiet()
		if n := h.rt.sendCallCount("s1"); n != i+1 {
			t.Fatalf("after boundary %d the session has been sent %d turns, want %d", i+1, n, i+1)
		}
	}
	for i, want := range []string{"first", "second", "third"} {
		if h.rt.sendCalls[i].Text != want {
			t.Fatalf("turn %d = %q, want %q", i, h.rt.sendCalls[i].Text, want)
		}
	}
}

func TestARuntimeThatRejectsMidTurnInputWaitsForIdle(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	rejections := 2
	h.onSend = func(string, string) error {
		if rejections > 0 {
			rejections--
			return fmt.Errorf("%w: busy", agentsessions.ErrTurnInFlight)
		}
		return nil
	}
	receipt, _ := h.reply(h.routed("s1").ID, "retry me", false)
	got := h.waitState(receipt.ReplyID, store.RoutingReplyDelivered)
	// Two rejections are waits, not failures: only the accepted turn counts.
	if got.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (a mid-turn rejection is not an attempt)", got.Attempts)
	}
	if h.rt.sendCallCount("s1") != 3 {
		t.Fatalf("send calls = %d", h.rt.sendCallCount("s1"))
	}
}

func TestSubmitFailureRetriesThenIsUndeliverableWithTheError(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	h.rt.setSendErr("s1", errors.New("stdin closed"))
	receipt, _ := h.reply(h.routed("s1").ID, "doomed", false)
	got := h.waitState(receipt.ReplyID, store.RoutingReplyUndeliverable)
	if got.Reason != ReplyReasonSubmitFailed || got.Detail != "stdin closed" || got.Attempts != replyMaxAttempts {
		t.Fatalf("undeliverable = %+v", got)
	}
	// Never dropped: the text is still stored and readable.
	if body, err := h.st.RoutingReplyBody(context.Background(), receipt.ReplyID); err != nil || body != "doomed" {
		t.Fatalf("body %q %v", body, err)
	}
}

func TestEndedSessionHandsOffToTheActorsBoundSession(t *testing.T) {
	h := newReplyHarness(t)
	ctx := context.Background()
	// s-old is the originator and is gone; the actor is now bound to s-new.
	if err := h.st.CreateSession(store.SessionRow{ID: "s-old", LogicalAgentID: "worker", State: "completed"}, nil); err != nil {
		t.Fatal(err)
	}
	h.rt.setAlive("s-new", true, agentsessions.LiveStateIdle)
	if _, err := h.reg.LeaseBinding(ctx, registry.LogicalAgentBindingTarget("worker"), "s-new", "local", "s-new", nil, "", 0); err != nil {
		t.Fatal(err)
	}
	receipt, err := h.reply(h.routed("s-old").ID, "continue on the new session", false)
	if err != nil {
		t.Fatal(err)
	}
	got := h.waitState(receipt.ReplyID, store.RoutingReplyDelivered)
	if got.OriginalSessionID != "s-old" || got.TargetSessionID != "s-new" || got.DeliveredToSessionID != "s-new" || got.Reason != ReplyReasonHandedOff {
		t.Fatalf("handed off = %+v", got)
	}
	if h.rt.sendCallCount("s-old") != 0 || h.rt.sendCallCount("s-new") != 1 {
		t.Fatalf("sends = %+v", h.rt.sendCalls)
	}
}

func TestEndedSessionWithoutABindingIsUndeliverableWithAReason(t *testing.T) {
	h := newReplyHarness(t)
	ctx := context.Background()
	if err := h.st.CreateSession(store.SessionRow{ID: "s-old", LogicalAgentID: "worker", State: "completed"}, nil); err != nil {
		t.Fatal(err)
	}
	// An unrelated running session of the same logical agent must NOT receive it:
	// handing off is the stable binding's job, not a newest-running scan.
	if err := h.st.CreateSession(store.SessionRow{ID: "s-other", LogicalAgentID: "worker", State: "running"}, nil); err != nil {
		t.Fatal(err)
	}
	h.rt.setAlive("s-other", true, agentsessions.LiveStateIdle)
	receipt, _ := h.reply(h.routed("s-old").ID, "nobody home", false)
	got := h.waitState(receipt.ReplyID, store.RoutingReplyUndeliverable)
	if got.Reason != ReplyReasonNoBinding || got.Detail == "" || got.SettledAt == nil {
		t.Fatalf("undeliverable = %+v", got)
	}
	if h.rt.sendCallCount("s-other") != 0 {
		t.Fatal("redirected to a session that is not the actor's binding")
	}
	// Visible to consumers through the service, and announced on the bus.
	view, err := h.svc.RoutingReplyDelivery(ctx, receipt.ReplyID)
	if err != nil || view.State != "undeliverable" || view.Reason != ReplyReasonNoBinding || view.OriginalSessionID != "s-old" {
		t.Fatalf("delivery view = %+v %v", view, err)
	}
	kinds, evs := h.waitEvents(1)
	if kinds[0] != events.KindRoutingReplyUndeliverable || evs[0].Reason != ReplyReasonNoBinding {
		t.Fatalf("events = %v %+v", kinds, evs)
	}
	if body, err := h.st.RoutingReplyBody(ctx, receipt.ReplyID); err != nil || body != "nobody home" {
		t.Fatalf("an undeliverable reply must stay readable: %q %v", body, err)
	}
}

func TestEndedSessionBindingProblemsEachGetTheirOwnReason(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		bind   func(h *replyHarness)
		reason string
	}{
		{"bound session not running", func(h *replyHarness) {
			if _, err := h.reg.LeaseBinding(ctx, registry.LogicalAgentBindingTarget("worker"), "s-dead", "local", "s-dead", nil, "", 0); err != nil {
				t.Fatal(err)
			}
		}, ReplyReasonBoundNotRunning},
		{"pull-only bridge", func(h *replyHarness) {
			h.rt.setAlive("s-bridge", true, agentsessions.LiveStateIdle)
			if _, err := h.reg.LeaseBinding(ctx, registry.LogicalAgentBindingTarget("worker"), "s-bridge", "external-bridge-host", "attempt-1",
				[]string{api.PullOnlyCapability}, registry.VisibilityPublishedLocal, 0); err != nil {
				t.Fatal(err)
			}
		}, ReplyReasonPullOnly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newReplyHarness(t)
			tc.bind(h)
			receipt, _ := h.reply(h.routed("s-old").ID, "x", false)
			got := h.waitState(receipt.ReplyID, store.RoutingReplyUndeliverable)
			if got.Reason != tc.reason {
				t.Fatalf("reason = %q, want %q (%+v)", got.Reason, tc.reason, got)
			}
			if h.rt.sendCallCount("s-bridge") != 0 {
				t.Fatal("injected a turn into a pull-only bridge")
			}
		})
	}
}

func TestQueuedRepliesOfASessionThatEndsAreResolvedByTheExitNotification(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	h.setBusy("s1", true)
	receipt, _ := h.reply(h.routed("s1").ID, "too late", false)
	settleQuiet()
	// The session exits mid-wait; flush() notifies.
	h.rt.setAlive("s1", false, agentsessions.LiveStateIdle)
	h.d.notify("s1")
	got := h.waitState(receipt.ReplyID, store.RoutingReplyUndeliverable)
	if got.Reason != ReplyReasonNoBinding {
		t.Fatalf("got %+v", got)
	}
}

func TestStartupRecovery(t *testing.T) {
	ctx := context.Background()
	t.Run("a delivering reply whose session still runs is not sent again", func(t *testing.T) {
		h := newReplyHarness(t)
		h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
		r, _, _ := h.st.CreateRoutingReply(ctx, store.NewRoutingReply{From: messaging.Address{Kind: messaging.KindUser, Authority: "local", ID: "c"},
			ParentID: "p", Body: "maybe sent", TargetSessionID: "s1", LogicalAgentID: "worker", Actor: "msg://user/local/c"})
		if ok, _ := h.st.ClaimRoutingReply(ctx, r.ReplyID); !ok {
			t.Fatal("claim")
		}
		if err := h.d.recover(ctx); err != nil {
			t.Fatal(err)
		}
		got := h.waitState(r.ReplyID, store.RoutingReplyUndeliverable)
		if got.Reason != ReplyReasonRestart || h.rt.sendCallCount("s1") != 0 {
			t.Fatalf("got %+v sends=%d", got, h.rt.sendCallCount("s1"))
		}
	})
	t.Run("a delivering reply whose session is gone follows the ended-session path", func(t *testing.T) {
		h := newReplyHarness(t)
		h.rt.setAlive("s-new", true, agentsessions.LiveStateIdle)
		if _, err := h.reg.LeaseBinding(ctx, registry.LogicalAgentBindingTarget("worker"), "s-new", "local", "s-new", nil, "", 0); err != nil {
			t.Fatal(err)
		}
		r, _, _ := h.st.CreateRoutingReply(ctx, store.NewRoutingReply{From: messaging.Address{Kind: messaging.KindUser, Authority: "local", ID: "c"},
			ParentID: "p", Body: "carry on", TargetSessionID: "s-gone", LogicalAgentID: "worker", Actor: "msg://user/local/c"})
		if ok, _ := h.st.ClaimRoutingReply(ctx, r.ReplyID); !ok {
			t.Fatal("claim")
		}
		if err := h.d.recover(ctx); err != nil {
			t.Fatal(err)
		}
		got := h.waitState(r.ReplyID, store.RoutingReplyDelivered)
		if got.DeliveredToSessionID != "s-new" || got.OriginalSessionID != "s-gone" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("queued replies are drained after a restart", func(t *testing.T) {
		h := newReplyHarness(t)
		h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
		r, _, _ := h.st.CreateRoutingReply(ctx, store.NewRoutingReply{From: messaging.Address{Kind: messaging.KindUser, Authority: "local", ID: "c"},
			ParentID: "p", Body: "waiting", TargetSessionID: "s1", LogicalAgentID: "worker", Actor: "msg://user/local/c"})
		if err := h.d.recover(ctx); err != nil {
			t.Fatal(err)
		}
		h.waitState(r.ReplyID, store.RoutingReplyDelivered)
	})
}

func TestPurgedBodyIsUndeliverableNotInvented(t *testing.T) {
	h := newReplyHarness(t)
	ctx := context.Background()
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	h.setBusy("s1", true)
	receipt, _ := h.reply(h.routed("s1").ID, "will be purged", false)
	// An operator purged the body while the reply waited.
	if _, err := h.st.DB().ExecContext(ctx, `UPDATE messages SET payload=NULL WHERE id=?`, receipt.ReplyID); err != nil {
		t.Fatal(err)
	}
	h.setBusy("s1", false)
	h.d.notify("s1")
	got := h.waitState(receipt.ReplyID, store.RoutingReplyUndeliverable)
	if got.Reason != ReplyReasonBodyPurged || h.rt.sendCallCount("s1") != 0 {
		t.Fatalf("got %+v sends=%d", got, h.rt.sendCallCount("s1"))
	}
}

// ─── submit: resolution, validation, authorization ──────────────────────────

func TestSubmitResolvesOnlyAMessageASessionRouted(t *testing.T) {
	h := newReplyHarness(t)
	ctx := context.Background()
	if _, err := h.reply("no-such-message", "x", false); !errors.Is(err, api.ErrReplyParentNotFound) {
		t.Fatalf("unknown parent: %v", err)
	}
	// A message from a user is not routed from a session.
	to, _ := channels.ChannelAddress("ops")
	human, _ := h.st.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice,
		From: messaging.Address{Kind: messaging.KindUser, Authority: "local", ID: "chris"}, To: to, Payload: []byte(`"hi"`)})
	if _, err := h.reply(human.ID, "x", false); !errors.Is(err, api.ErrReplyTargetNotSession) {
		t.Fatalf("user sender: %v", err)
	}
	// A routed message whose recorded session disagrees with its sender is refused, not guessed.
	forged, _ := h.st.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice,
		From: messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "s1"}, To: to, Payload: []byte(`"hi"`),
		Metadata: map[string]string{"session_id": "s2"}})
	if _, err := h.reply(forged.ID, "x", false); !errors.Is(err, api.ErrReplyTargetNotSession) {
		t.Fatalf("session_id mismatch: %v", err)
	}
	if rows, _ := h.st.RoutingRepliesInState(ctx, store.RoutingReplyQueued, 10); len(rows) != 0 {
		t.Fatalf("refused replies must not queue: %+v", rows)
	}
}

func TestSubmitValidatesTheBodyAndTheCaller(t *testing.T) {
	h := newReplyHarness(t)
	parent := h.routed("s1")
	if _, err := h.reply(parent.ID, "   ", false); !errors.Is(err, api.ErrReplyInvalid) {
		t.Fatalf("blank: %v", err)
	}
	if _, err := h.reply(parent.ID, string(make([]byte, replyMaxBodyBytes+1)), false); !errors.Is(err, api.ErrReplyTooLarge) {
		t.Fatalf("too large: %v", err)
	}
	if _, err := h.reply(parent.ID, "bad \xff utf8", false); !errors.Is(err, api.ErrReplyInvalid) {
		t.Fatalf("utf8: %v", err)
	}
	_, err := h.svc.SubmitRoutingReply(context.Background(), api.RoutingReplyRequest{ParentID: parent.ID, Body: "x", Caller: identity.Principal{ID: "not a urn"}})
	if !errors.Is(err, api.ErrReplyInvalid) {
		t.Fatalf("caller: %v", err)
	}
}

func TestReplyAuthorizationHookIsTheEnforcementSeam(t *testing.T) {
	h := newReplyHarness(t)
	parent := h.routed("s1")
	h.rt.setAlive("s1", true, agentsessions.LiveStateProcessing)
	// Observe mode: an unverified caller is accepted.
	if _, err := h.svc.SubmitRoutingReply(context.Background(), api.RoutingReplyRequest{ParentID: parent.ID, Body: "ok",
		Caller: identity.Principal{ID: "msg://user/local/asserted"}}); err != nil {
		t.Fatalf("observe mode refused: %v", err)
	}
	var sawTarget string
	h.svc.ReplyAuthorization = func(_ context.Context, caller identity.Principal, p messaging.Envelope, target string) error {
		sawTarget = target
		if caller.ID != "msg://user/local/chris" {
			return errors.New("not you")
		}
		return nil
	}
	if _, err := h.svc.SubmitRoutingReply(context.Background(), api.RoutingReplyRequest{ParentID: parent.ID, Body: "no",
		Caller: identity.Principal{ID: "msg://user/local/mallory"}}); !errors.Is(err, api.ErrReplyForbidden) {
		t.Fatalf("hook refusal: %v", err)
	}
	if _, err := h.reply(parent.ID, "yes", false); err != nil || sawTarget != "s1" {
		t.Fatalf("hook accept: %v target=%q", err, sawTarget)
	}
	if rows, _ := h.st.RoutingRepliesInState(context.Background(), store.RoutingReplyQueued, 10); len(rows) != 2 {
		t.Fatalf("a refused reply must not queue; queued = %d", len(rows))
	}
}

func TestSubmitIsRefusedWhenReplyRoutingIsNotInstalled(t *testing.T) {
	st, reg := newWakeHarness(t)
	svc := &Service{Store: st, Registry: reg}
	if svc.RoutingReplyWired() {
		t.Fatal("wired before StartRoutingReplies")
	}
	_, err := svc.SubmitRoutingReply(context.Background(), api.RoutingReplyRequest{ParentID: "x", Body: "y", Caller: identity.Principal{ID: "msg://user/local/c"}})
	if !errors.Is(err, api.ErrRoutingRepliesNotWired) {
		t.Fatalf("got %v", err)
	}
	if err := svc.StartRoutingReplies(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !svc.RoutingReplyWired() {
		t.Fatal("not wired after StartRoutingReplies")
	}
	if err := svc.StartRoutingReplies(context.Background()); err != nil {
		t.Fatalf("second start: %v", err)
	}
	svc.stopRoutingReplies()
	if svc.RoutingReplyWired() {
		t.Fatal("still wired after stop")
	}
}

func TestIdempotentRetryReturnsTheSameReplyAndDeliversOnce(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	parent := h.routed("s1")
	req := api.RoutingReplyRequest{ParentID: parent.ID, Body: "once", Verified: true, IdempotencyKey: "k",
		Caller: identity.Principal{ID: "msg://user/local/chris"}}
	first, err := h.svc.SubmitRoutingReply(context.Background(), req)
	if err != nil || first.Duplicate {
		t.Fatalf("first: %+v %v", first, err)
	}
	second, err := h.svc.SubmitRoutingReply(context.Background(), req)
	if err != nil || !second.Duplicate || second.ReplyID != first.ReplyID {
		t.Fatalf("retry: %+v %v", second, err)
	}
	h.waitState(first.ReplyID, store.RoutingReplyDelivered)
	settleQuiet()
	if n := h.rt.sendCallCount("s1"); n != 1 {
		t.Fatalf("delivered %d times", n)
	}
	req.Body = "different"
	if _, err := h.svc.SubmitRoutingReply(context.Background(), req); !errors.Is(err, api.ErrReplyIdempotencyConflict) {
		t.Fatalf("same key, different body: %v", err)
	}
}

// ─── interrupt ──────────────────────────────────────────────────────────────

func TestInterruptCancelsTheTurnThenDeliversTheReplyAsTheNextTurn(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	h.setBusy("s1", true)
	h.intr.onCancel = func(id string) {
		// The turn ends: the settle hook clears the marker and notifies.
		h.setBusy(id, false)
		h.d.notify(id)
	}
	receipt, err := h.reply(h.routed("s1").ID, "stop, do this instead", true)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Interrupt != "cancelled" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if h.intr.callCount() != 1 || h.intr.calls[0] != "s1|msg://user/local/chris" {
		t.Fatalf("interrupter calls = %v (the caller must be the audited actor)", h.intr.calls)
	}
	got := h.waitState(receipt.ReplyID, store.RoutingReplyDelivered)
	if !got.Interrupt || h.rt.sendCalls[0].Text != "stop, do this instead" {
		t.Fatalf("got %+v sends %+v", got, h.rt.sendCalls)
	}
}

func TestInterruptWithNothingToCancelIsAPlainNextTurnDelivery(t *testing.T) {
	for reason, want := range map[TurnInterruptRefusalReason]string{
		TurnInterruptNoTurn:     "no_turn_in_progress",
		TurnInterruptSuperseded: "turn_superseded",
	} {
		t.Run(string(reason), func(t *testing.T) {
			h := newReplyHarness(t)
			h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
			h.intr.err = &TurnInterruptRefusal{Reason: reason, SessionID: "s1"}
			receipt, err := h.reply(h.routed("s1").ID, "next", true)
			if err != nil || receipt.Interrupt != want {
				t.Fatalf("receipt %+v err %v", receipt, err)
			}
			h.waitState(receipt.ReplyID, store.RoutingReplyDelivered)
		})
	}
	t.Run("the session already ended", func(t *testing.T) {
		h := newReplyHarness(t)
		h.intr.err = agentsessions.ErrSessionNotRunning
		receipt, err := h.reply(h.routed("s-gone").ID, "next", true)
		if err != nil || receipt.Interrupt != "session_not_running" {
			t.Fatalf("receipt %+v err %v", receipt, err)
		}
		// It is accepted and then resolved like any reply to an ended session.
		h.waitState(receipt.ReplyID, store.RoutingReplyUndeliverable)
	})
}

func TestInterruptRefusalsAcceptNothing(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"runtime cannot cancel", &TurnInterruptRefusal{Reason: TurnInterruptUnsupported, SessionID: "s1"}, api.ErrReplyInterruptUnsupported},
		{"the adapter reports it", agentsessions.ErrInterruptUnsupported, api.ErrReplyInterruptUnsupported},
		{"turn submitted but not started", &TurnInterruptRefusal{Reason: TurnInterruptNotStarted, SessionID: "s1"}, api.ErrReplyTurnNotStarted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newReplyHarness(t)
			h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
			h.intr.err = tc.err
			_, err := h.reply(h.routed("s1").ID, "x", true)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if rows, _ := h.st.RoutingRepliesInState(context.Background(), store.RoutingReplyQueued, 10); len(rows) != 0 {
				t.Fatalf("a refused interrupt must not queue the reply: %+v", rows)
			}
			settleQuiet()
			if h.rt.sendCallCount("s1") != 0 {
				t.Fatal("delivered a refused reply")
			}
		})
	}
	t.Run("no interrupter installed", func(t *testing.T) {
		h := newReplyHarness(t)
		h.svc.interrupter = nil
		if _, err := h.reply(h.routed("s1").ID, "x", true); !errors.Is(err, api.ErrReplyInterruptUnsupported) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a plain reply never asks the interrupter", func(t *testing.T) {
		h := newReplyHarness(t)
		h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
		h.intr.err = errors.New("must not be called")
		receipt, err := h.reply(h.routed("s1").ID, "x", false)
		if err != nil || h.intr.callCount() != 0 {
			t.Fatalf("%+v %v calls=%d", receipt, err, h.intr.callCount())
		}
	})
}

func TestRetryOfAnInterruptingReplyDoesNotCancelTheTurnItStarted(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	parent := h.routed("s1")
	req := api.RoutingReplyRequest{ParentID: parent.ID, Body: "now", Interrupt: true, Verified: true, IdempotencyKey: "k",
		Caller: identity.Principal{ID: "msg://user/local/chris"}}
	first, err := h.svc.SubmitRoutingReply(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// The reply's own turn is now running; a client retry must not cancel it.
	retry, err := h.svc.SubmitRoutingReply(context.Background(), req)
	if err != nil || !retry.Duplicate || retry.ReplyID != first.ReplyID {
		t.Fatalf("retry %+v %v", retry, err)
	}
	if n := h.intr.callCount(); n != 1 {
		t.Fatalf("interrupter called %d times", n)
	}
}

func TestConcurrentInterruptingRetriesCancelOnce(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	parent := h.routed("s1")
	req := api.RoutingReplyRequest{ParentID: parent.ID, Body: "now", Interrupt: true, Verified: true, IdempotencyKey: "k",
		Caller: identity.Principal{ID: "msg://user/local/chris"}}
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := h.svc.SubmitRoutingReply(context.Background(), req)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- r.ReplyID
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 || h.intr.callCount() != 1 {
		t.Fatalf("replies=%d interrupts=%d, want 1 and 1", len(seen), h.intr.callCount())
	}
}

// ─── dispatcher mechanics ───────────────────────────────────────────────────

func TestKicksDuringADrainAreNotLost(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	h.onSend = func(id, text string) error {
		entered <- struct{}{}
		if text == "first" {
			<-release // a blocking runtime: SendTurn returns after the whole turn
		}
		return nil
	}
	parent := h.routed("s1")
	first, _ := h.reply(parent.ID, "first", false)
	<-entered
	// While the first turn is being submitted, a second reply arrives and the
	// turn's own completion notifies.
	second, _ := h.reply(parent.ID, "second", false)
	h.d.notify("s1")
	close(release)
	h.waitState(first.ReplyID, store.RoutingReplyDelivered)
	h.waitState(second.ReplyID, store.RoutingReplyDelivered)
}

func TestStopWaitsForDrainsAndRefusesNewWork(t *testing.T) {
	h := newReplyHarness(t)
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	h.d.stop()
	h.d.notify("s1") // must be a no-op, not a panic or a goroutine leak
	r, _, _ := h.st.CreateRoutingReply(context.Background(), store.NewRoutingReply{From: messaging.Address{Kind: messaging.KindUser, Authority: "local", ID: "c"},
		ParentID: "p", Body: "x", TargetSessionID: "s1", Actor: "msg://user/local/c"})
	h.d.notify("s1")
	settleQuiet()
	if got := h.state(r.ReplyID); got.State != store.RoutingReplyQueued || h.rt.sendCallCount("s1") != 0 {
		t.Fatalf("a stopped dispatcher delivered: %+v", got)
	}
}
