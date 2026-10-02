package app

// Reply-to-sender (CW-20261002-0065, ADR 0049 s1.6).
//
// A reply to a message a session routed to a channel is queued here and
// injected, as that session's NEXT TURN, at its next idle boundary. Tether is
// the single writer: the reply row is stored without any inbox, delivery-core
// or wake obligation, so wake.go's claim/Nack poll never sees it and a
// consumer never calls SendTurn. The routing_replies table is the queue.
//
// Triggers, none of them a poll of the reply itself:
//   - a reply is accepted for an idle session;
//   - the session's turn settles (sessionTurnOutput.settleTurn / flush call
//     notifyReplyIdle), which covers completion, a failed submission, an
//     interrupt and session exit;
//   - a repair sweep and startup recovery, for runtimes with no turn feed
//     (PTY) and for rows whose retry time has arrived.
//
// One reply per idle boundary. A session that ended hands its queue to the
// session its actor is bound to (never a newest-running scan), or marks each
// reply undeliverable with a reason consumers can read.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hollis-labs/agentkit/agentsessions"
	llmtypes "github.com/hollis-labs/go-llm-types"
	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-runner/runner"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

// Reasons a reply is requeued or undeliverable. Consumers key on these.
const (
	ReplyReasonNoBinding       = "session_ended_no_binding"
	ReplyReasonBoundNotRunning = "bound_session_not_running"
	ReplyReasonPullOnly        = "pull_only_binding"
	ReplyReasonResolveFailed   = "resolve_failed"
	ReplyReasonSubmitFailed    = "submit_failed"
	ReplyReasonRestart         = "daemon_restarted_during_delivery"
	ReplyReasonBodyPurged      = "body_purged"
	ReplyReasonHandedOff       = "handed_off"
	ReplyReasonWaitingForIdle  = "waiting_for_idle"
	ReplyReasonTurnFailed      = "turn_failed"
	ReplyReasonNoTurnFeed      = "no_turn_feed"
	// ReplyReasonInterruptUnconfirmed: the daemon stopped between reserving an
	// interrupting reply and learning whether the cancel happened. The caller never
	// got a receipt, so the reply is not sent; resubmit it.
	ReplyReasonInterruptUnconfirmed = "interrupt_unconfirmed"
)

const (
	replyMaxBodyBytes = 128 << 10
	replyMaxAttempts  = 5
	replyDrainBatch   = 50
)

// Vars so tests can shrink them instead of waiting out production backoffs.
var (
	replySubmitBackoff = 2 * time.Second
	replyIdleBackoff   = time.Second
)

// Interrupt outcomes reported on the receipt. The cancel outcome is the same
// string as a turn_output stop_reason, so a consumer can join the two.
const (
	replyInterruptCancelled  = llmtypes.StopReasonCancelled
	replyInterruptNoTurn     = "no_turn_in_progress"
	replyInterruptSuperseded = "turn_superseded"
	replyInterruptNotRunning = "session_not_running"
	// The cancel was requested but the turn did not end in time. The reply is
	// accepted and follows the turn's eventual boundary like any queued reply.
	replyInterruptTimedOut = "interrupt_timeout"
)

// ReplyAuthorization is the caller-identity hook for replies, shaped like the
// channel Authorization. Nil is observe mode (ADR 0045): the verified principal
// (else the self-asserted ?as=) is recorded as the actor and nothing is
// refused. CW-20260930-0253 phases 2-3 install a checker here.
type ReplyAuthorization func(ctx context.Context, caller identity.Principal, parent messaging.Envelope, targetSessionID string) error

// turnInterrupter is Service.CancelTurnAndWait (CW-20261002-0067); tests substitute a fake.
type turnInterrupter interface {
	CancelTurnAndWait(ctx context.Context, sessionID, actor string) (TurnInterruptResult, error)
}

// replyRuntime is the process-layer seam: wakeRuntime's health and sendTurn
// plus whether Tether's own marker says a turn is in flight, and whether the
// session's runtime reports turns at all.
type replyRuntime struct {
	wakeRuntime
	turnBusy func(sessionID string) bool
	// noTurnFeed reports a runtime with no turn lifecycle (a PTY): nothing ever
	// says when it is idle, so a reply could not be delivered at a boundary.
	noTurnFeed func(sessionID string) bool
}

type replyDrain struct {
	running bool
	// again: a boundary kick (turn settled, exit, timer, sweep) arrived while a
	// drain ran. fresh: a new reply was queued while a drain ran. submitted: the
	// last drain handed the session a turn, so only a boundary may license
	// another; a new reply says nothing about the session being idle.
	again, fresh, submitted bool
	// submit serializes interrupt:true submissions to one session, so a retry
	// of an interrupting reply cannot cancel the turn its twin just started.
	// submitters counts holders and waiters so the entry outlives them.
	submit     sync.Mutex
	submitters int
}

type replyDispatcher struct {
	st     *store.Store
	reg    *registry.Service
	rt     replyRuntime
	pub    func(kind string, ev events.RoutingReplyEvent)
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	sessions map[string]*replyDrain
	stopped  bool
	// inflight holds the replies this process has claimed and not yet settled or
	// requeued. A 'delivering' row that is not in it was left by a process that
	// is gone and is resolved by resolveStale.
	inflight map[string]struct{}
}

func newReplyDispatcher(ctx context.Context, st *store.Store, reg *registry.Service, rt replyRuntime, pub func(string, events.RoutingReplyEvent)) *replyDispatcher {
	ctx, cancel := context.WithCancel(ctx)
	return &replyDispatcher{st: st, reg: reg, rt: rt, pub: pub, ctx: ctx, cancel: cancel, sessions: map[string]*replyDrain{}, inflight: map[string]struct{}{}}
}

// drainFor returns sessionID's drain state; call with d.mu held.
func (d *replyDispatcher) drainFor(sessionID string) *replyDrain {
	st, ok := d.sessions[sessionID]
	if !ok {
		st = &replyDrain{}
		d.sessions[sessionID] = st
	}
	return st
}

// notify reports a boundary for sessionID, or a repair tick: its turn settled, it
// exited, a retry time arrived, or the sweep ran. It asks for the queue to be
// drained. It never blocks and takes no session or reducer lock, so it is safe
// from a runtime callback. A boundary that arrives while a drain is running is
// remembered and re-run, never lost.
func (d *replyDispatcher) notify(sessionID string) { d.kick(sessionID, true) }

// notifyNew reports that a reply was queued for sessionID. It starts a drain,
// but once a drain has injected a turn, a new reply does not license another:
// only a boundary does.
func (d *replyDispatcher) notifyNew(sessionID string) { d.kick(sessionID, false) }

func (d *replyDispatcher) kick(sessionID string, boundary bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped || sessionID == "" {
		return
	}
	st := d.drainFor(sessionID)
	if st.running || st.submitters > 0 {
		// Running: the loop re-runs on the flag. Held by an interrupting
		// submission: nothing may be delivered until it has queued its reply; its
		// release notifies. Either way the kick is remembered, never lost.
		if boundary {
			st.again = true
		} else {
			st.fresh = true
		}
		return
	}
	st.running = true
	d.wg.Add(1)
	go d.drainLoop(sessionID)
}

func (d *replyDispatcher) drainLoop(sessionID string) {
	defer d.wg.Done()
	for {
		d.mu.Lock()
		st := d.drainFor(sessionID)
		st.again, st.fresh = false, false
		d.mu.Unlock()

		delivered := d.drain(sessionID)

		d.mu.Lock()
		st = d.drainFor(sessionID)
		st.submitted = delivered
		if (st.again || (st.fresh && !st.submitted)) && !d.stopped {
			d.mu.Unlock()
			continue
		}
		st.running, st.again, st.fresh, st.submitted = false, false, false, false
		if st.submitters == 0 {
			delete(d.sessions, sessionID)
		}
		d.mu.Unlock()
		return
	}
}

// lockSubmit serializes interrupting submissions to sessionID until release is
// called, and holds the session's queue meanwhile: the cancel ends the running
// turn, and that boundary must not hand the next turn to an older reply before
// the interrupting one has been queued ahead of it. Release lifts the hold and
// notifies, so whatever was held back is drained then.
func (d *replyDispatcher) lockSubmit(sessionID string) (release func()) {
	d.mu.Lock()
	st := d.drainFor(sessionID)
	st.submitters++
	d.mu.Unlock()
	st.submit.Lock()
	return func() {
		st.submit.Unlock()
		d.mu.Lock()
		st.submitters--
		if st.submitters == 0 && !st.running && !st.again && !st.fresh {
			delete(d.sessions, sessionID)
		}
		d.mu.Unlock()
		d.notify(sessionID)
	}
}

func (d *replyDispatcher) held(sessionID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.sessions[sessionID]
	return ok && st.submitters > 0
}

func (d *replyDispatcher) track(replyID string) {
	d.mu.Lock()
	d.inflight[replyID] = struct{}{}
	d.mu.Unlock()
}

func (d *replyDispatcher) untrack(replyID string) {
	d.mu.Lock()
	delete(d.inflight, replyID)
	d.mu.Unlock()
}

func (d *replyDispatcher) isInflight(replyID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.inflight[replyID]
	return ok
}

func (d *replyDispatcher) stop() {
	d.mu.Lock()
	d.stopped = true
	d.mu.Unlock()
	d.cancel()
	d.wg.Wait()
}

func (d *replyDispatcher) busy(sessionID string) bool {
	if h, ok := d.rt.health(sessionID); ok && h.Health.State == agentsessions.LiveStateProcessing {
		return true
	}
	return d.rt.turnBusy != nil && d.rt.turnBusy(sessionID)
}

// drain delivers at most one reply to sessionID, if it is idle, or resolves its
// whole queue if the session is gone. It reports whether it injected a turn.
// Only the head of the queue is ever delivered: a younger reply that is due does
// not jump an older one that is backing off.
func (d *replyDispatcher) drain(sessionID string) bool {
	for d.ctx.Err() == nil {
		if d.held(sessionID) {
			return false // an interrupting submission is mid-flight; its release notifies
		}
		rows, err := d.st.QueuedRoutingReplies(d.ctx, sessionID, replyDrainBatch)
		if err != nil {
			if d.ctx.Err() == nil {
				log.Printf("routing reply: read queue of session %q: %v", sessionID, err)
			}
			return false
		}
		if len(rows) == 0 {
			return false
		}
		if _, running := d.rt.health(sessionID); !running {
			d.resolveEnded(sessionID, rows)
			return false
		}
		if d.rt.noTurnFeed != nil && d.rt.noTurnFeed(sessionID) {
			for _, r := range rows {
				d.settle(r, store.RoutingReplySettlement{State: store.RoutingReplyUndeliverable, Reason: ReplyReasonNoTurnFeed,
					Detail: "the session's runtime reports no turn lifecycle (a PTY), so there is no idle boundary to deliver at"})
			}
			return false
		}
		if d.busy(sessionID) {
			return false // the turn's completion notifies again
		}
		r := rows[0]
		if r.NextAttemptAt != nil && r.NextAttemptAt.After(time.Now()) {
			return false // the head is backing off; its timer (and the sweep) notify again
		}
		d.track(r.ReplyID)
		if d.held(sessionID) {
			d.untrack(r.ReplyID)
			return false
		}
		claimed, err := d.st.ClaimRoutingReply(d.ctx, r.ReplyID)
		if err != nil {
			d.untrack(r.ReplyID)
			log.Printf("routing reply %s: claim: %v", r.ReplyID, err)
			return false
		}
		if !claimed {
			d.untrack(r.ReplyID)
			continue // another drain took it; look at the next
		}
		d.deliver(sessionID, r)
		return true
	}
	return false
}

// replyTurnRan reports a submission error that came back after the runtime took
// the turn and ran it. The model has acted on the reply; repeating it would run
// it twice.
func replyTurnRan(err error) bool {
	var ran *turnRanError
	var exit *runner.ExitError
	return errors.As(err, &ran) || errors.As(err, &exit)
}

// deliver injects r's body as sessionID's next turn. r was claimed, so r.Attempts
// is stale by one: the stored attempt count is authoritative.
func (d *replyDispatcher) deliver(sessionID string, r store.RoutingReply) {
	defer d.untrack(r.ReplyID)
	body, err := d.st.RoutingReplyBody(d.ctx, r.ReplyID)
	if err != nil {
		if errors.Is(err, store.ErrRoutingReplyBodyPurged) {
			d.settle(r, store.RoutingReplySettlement{State: store.RoutingReplyUndeliverable, Reason: ReplyReasonBodyPurged,
				Detail: "the reply text was purged before it could be delivered"})
			return
		}
		d.requeue(r, store.RoutingReplyRequeue{Reason: ReplyReasonSubmitFailed, Detail: err.Error(), NotBefore: time.Now().Add(replySubmitBackoff)})
		return
	}
	err = d.rt.sendTurn(d.ctx, sessionID, body)
	switch {
	case err == nil:
		reason := ""
		if sessionID != r.OriginalSessionID {
			reason = ReplyReasonHandedOff
		}
		d.settle(r, store.RoutingReplySettlement{State: store.RoutingReplyDelivered, Reason: reason, DeliveredTo: sessionID})
	case d.ctx.Err() != nil:
		// Shutting down mid-submit: whether the runtime took it is unknown. The
		// row stays 'delivering' and startup recovery resolves it at-most-once.
	case replyTurnRan(err):
		// A subprocess runtime blocks for the whole turn and returns the process's
		// failure afterwards: the reply was delivered and acted on, and the turn
		// failed. Report it; never run the reply again.
		reason := ReplyReasonTurnFailed
		d.settle(r, store.RoutingReplySettlement{State: store.RoutingReplyDelivered, Reason: reason, Detail: err.Error(), DeliveredTo: sessionID})
	case errors.Is(err, agentsessions.ErrTurnInFlight):
		// A runtime that rejects mid-turn input (OpenCode, ACP): not a failure,
		// wait for the turn's completion. The attempt does not count.
		d.requeue(r, store.RoutingReplyRequeue{Reason: ReplyReasonWaitingForIdle, RefundAttempt: true, NotBefore: time.Now().Add(replyIdleBackoff)})
	case errors.Is(err, agentsessions.ErrSessionNotRunning):
		// The Manager may not have noticed the exit yet: look again shortly.
		d.requeue(r, store.RoutingReplyRequeue{Reason: ReplyReasonSubmitFailed, Detail: err.Error(), RefundAttempt: true,
			NotBefore: time.Now().Add(replyIdleBackoff)})
	default:
		if cur, getErr := d.st.RoutingReply(d.ctx, r.ReplyID); getErr == nil {
			r.Attempts = cur.Attempts
		}
		if r.Attempts >= replyMaxAttempts {
			d.settle(r, store.RoutingReplySettlement{State: store.RoutingReplyUndeliverable, Reason: ReplyReasonSubmitFailed, Detail: err.Error()})
			return
		}
		d.requeue(r, store.RoutingReplyRequeue{Reason: ReplyReasonSubmitFailed, Detail: err.Error(),
			NotBefore: time.Now().Add(time.Duration(r.Attempts) * replySubmitBackoff)})
	}
}

// spawn runs fn on a tracked goroutine unless the dispatcher is stopping.
func (d *replyDispatcher) spawn(fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		fn()
	}()
}

// notifyAt drains sessionID's queue when at arrives, so a backed-off reply is
// retried on time rather than at the next repair sweep.
func (d *replyDispatcher) notifyAt(sessionID string, at time.Time) {
	d.spawn(func() {
		timer := time.NewTimer(time.Until(at))
		defer timer.Stop()
		select {
		case <-d.ctx.Done():
		case <-timer.C:
			d.notify(sessionID)
		}
	})
}

// resolveEnded settles or hands off every reply queued on a session that is no
// longer running. Only the actor's current binding is a successor; a lapsed or
// pull-only binding never falls through to some other running session.
func (d *replyDispatcher) resolveEnded(sessionID string, rows []store.RoutingReply) {
	for _, r := range rows {
		reason, detail, successor := d.successor(sessionID, r)
		switch {
		case successor != "":
			d.requeue(r, store.RoutingReplyRequeue{Retarget: successor, Reason: ReplyReasonHandedOff,
				Detail: fmt.Sprintf("session %s ended; its actor %s is bound to %s", sessionID, r.LogicalAgentID, successor)})
			d.notifyNew(successor)
		case reason == ReplyReasonResolveFailed && r.Attempts < replyMaxAttempts:
			// The binding lookup itself failed: retry, counting the attempt.
			if claimed, err := d.st.ClaimRoutingReply(d.ctx, r.ReplyID); err == nil && claimed {
				d.requeue(r, store.RoutingReplyRequeue{Reason: reason, Detail: detail, NotBefore: time.Now().Add(replySubmitBackoff)})
			}
		default:
			d.settle(r, store.RoutingReplySettlement{State: store.RoutingReplyUndeliverable, Reason: reason, Detail: detail})
		}
	}
}

// successor names the running session that now owns r's actor, or a reason it
// has none.
func (d *replyDispatcher) successor(endedSession string, r store.RoutingReply) (reason, detail, session string) {
	if r.LogicalAgentID == "" || d.reg == nil {
		return ReplyReasonNoBinding, "the session ended and the reply has no actor to hand it to", ""
	}
	b, err := d.reg.CurrentBinding(d.ctx, registry.LogicalAgentBindingTarget(r.LogicalAgentID))
	switch {
	case errors.Is(err, registry.ErrBindingNotFound):
		return ReplyReasonNoBinding, fmt.Sprintf("session %s ended and actor %s has no current binding", endedSession, r.LogicalAgentID), ""
	case err != nil:
		return ReplyReasonResolveFailed, err.Error(), ""
	case isPullOnly(b):
		return ReplyReasonPullOnly, fmt.Sprintf("actor %s is bound to a pull-only bridge; Tether cannot inject a turn", r.LogicalAgentID), ""
	case b.SessionID == endedSession:
		return ReplyReasonBoundNotRunning, fmt.Sprintf("actor %s is still bound to the ended session %s", r.LogicalAgentID, endedSession), ""
	}
	if _, ok := d.rt.health(b.SessionID); !ok {
		return ReplyReasonBoundNotRunning, fmt.Sprintf("actor %s is bound to session %s, which is not running", r.LogicalAgentID, b.SessionID), ""
	}
	return "", "", b.SessionID
}

func (d *replyDispatcher) requeue(r store.RoutingReply, in store.RoutingReplyRequeue) {
	in.Detail = store.BoundRoutingDetail(in.Detail)
	if err := d.st.RequeueRoutingReply(context.WithoutCancel(d.ctx), r.ReplyID, in); err != nil {
		if !errors.Is(err, store.ErrRoutingReplyState) {
			log.Printf("routing reply %s: requeue: %v", r.ReplyID, err)
		}
		return
	}
	if !in.NotBefore.IsZero() {
		target := in.Retarget
		if target == "" {
			target = r.TargetSessionID
		}
		d.notifyAt(target, in.NotBefore)
	}
}

func (d *replyDispatcher) settle(r store.RoutingReply, in store.RoutingReplySettlement) {
	in.Detail = store.BoundRoutingDetail(in.Detail)
	if err := d.st.SettleRoutingReply(context.WithoutCancel(d.ctx), r.ReplyID, in); err != nil {
		if !errors.Is(err, store.ErrRoutingReplyState) {
			log.Printf("routing reply %s: settle: %v", r.ReplyID, err)
		}
		return
	}
	if d.pub == nil {
		return
	}
	kind := events.KindRoutingReplyDelivered
	if in.State == store.RoutingReplyUndeliverable {
		kind = events.KindRoutingReplyUndeliverable
	}
	d.pub(kind, events.RoutingReplyEvent{ReplyID: r.ReplyID, ParentID: r.ParentID, State: string(in.State), Reason: in.Reason,
		Detail: in.Detail, OriginalSessionID: r.OriginalSessionID, TargetSessionID: r.TargetSessionID,
		DeliveredToSessionID: in.DeliveredTo, LogicalAgentID: r.LogicalAgentID, Actor: r.Actor})
}

// sweep resolves stale rows, then drains every session that has a queued reply.
// It is the repair path for runtimes with no turn feed, for rows whose retry
// time has arrived, and for rows a dead drain left behind.
func (d *replyDispatcher) sweep(ctx context.Context) (int, error) {
	if err := d.resolveStale(ctx); err != nil {
		return 0, err
	}
	sessions, err := d.st.SessionsWithQueuedRoutingReplies(ctx)
	if err != nil {
		return 0, err
	}
	for _, id := range sessions {
		d.notify(id)
	}
	return len(sessions), nil
}

// resolveStale settles rows no live drain or submission in this process owns.
//
// A 'delivering' row left by a previous process: whether the runtime took the
// turn is unknown, so a reply whose session is still running is NOT sent again
// (at-most-once per session): it becomes undeliverable with ReplyReasonRestart.
// If the session is gone the row goes back to the queue and follows the
// ended-session path to a bound successor.
//
// A 'pending' row is an interrupting reply reserved while its cancel was in
// flight; nobody was told it was queued and nothing says whether the cancel
// happened, so it is settled undeliverable (ReplyReasonInterruptUnconfirmed)
// rather than sent. A pending row for a session an interrupting submission
// currently holds belongs to that submission and is left alone.
func (d *replyDispatcher) resolveStale(ctx context.Context) error {
	delivering, err := d.st.RoutingRepliesInState(ctx, store.RoutingReplyDelivering, 1000)
	if err != nil {
		return err
	}
	for _, r := range delivering {
		if d.isInflight(r.ReplyID) {
			continue
		}
		if _, running := d.rt.health(r.TargetSessionID); running {
			d.settle(r, store.RoutingReplySettlement{State: store.RoutingReplyUndeliverable, Reason: ReplyReasonRestart,
				Detail: "the daemon restarted while this reply was being injected; it may or may not have reached the session, so it was not sent again"})
			continue
		}
		d.requeue(r, store.RoutingReplyRequeue{Reason: ReplyReasonRestart, Detail: "requeued after a daemon restart; the session it was being injected into is gone"})
		d.notify(r.TargetSessionID)
	}
	pending, err := d.st.RoutingRepliesInState(ctx, store.RoutingReplyPending, 1000)
	if err != nil {
		return err
	}
	for _, r := range pending {
		if d.held(r.TargetSessionID) {
			continue
		}
		d.settle(r, store.RoutingReplySettlement{State: store.RoutingReplyUndeliverable, Reason: ReplyReasonInterruptUnconfirmed,
			Detail: "the daemon stopped while this interrupting reply was being reserved; whether the turn was canceled is unknown, so the reply was not sent"})
	}
	return nil
}

// recover runs before the dispatcher is installed: stale rows are resolved and
// whatever is queued starts draining.
func (d *replyDispatcher) recover(ctx context.Context) error {
	_, err := d.sweep(ctx)
	return err
}

// ─── Service surface ─────────────────────────────────────────────────────────

// StartRoutingReplies installs the reply dispatcher: it recovers rows a previous
// process left mid-delivery, then drains whatever is queued. Until it is called
// RoutingReplyWired is false and replies are refused. Idempotent. ctx bounds the
// dispatcher's life.
func (s *Service) StartRoutingReplies(ctx context.Context) error {
	if s.Store == nil {
		return errors.New("routing replies need a store")
	}
	if s.replies.Load() != nil {
		return nil
	}
	d := newReplyDispatcher(ctx, s.Store, s.Registry, s.replyRuntime(), s.publishReplyEvent)
	// Recover before installing: a daemon whose recovery failed must not report
	// the reply path wired, and a reply must not be accepted onto rows still
	// 'delivering' from the last process.
	if err := d.recover(ctx); err != nil {
		d.stop()
		return fmt.Errorf("recover routing replies: %w", err)
	}
	if !s.replies.CompareAndSwap(nil, d) {
		d.stop()
		return nil
	}
	if s.interrupter == nil {
		s.interrupter = s
	}
	return nil
}

// RoutingReplyWired reports whether the reply path is installed: the dispatcher
// is running and a reply to a routed message will be delivered. False until
// StartRoutingReplies; the routing capabilities endpoint reports it.
func (s *Service) RoutingReplyWired() bool { return s.replies.Load() != nil }

// RunRoutingReplySweep is the repair pass the daemon ticks.
func (s *Service) RunRoutingReplySweep(ctx context.Context) (int, error) {
	d := s.replies.Load()
	if d == nil {
		return 0, nil
	}
	return d.sweep(ctx)
}

func (s *Service) stopRoutingReplies() {
	if d := s.replies.Swap(nil); d != nil {
		d.stop()
	}
}

// notifyReplyIdle is called when a session's turn settles or the session ends.
func (s *Service) notifyReplyIdle(sessionID string) {
	if d := s.replies.Load(); d != nil {
		d.notify(sessionID)
	}
}

func (s *Service) replyRuntime() replyRuntime {
	return replyRuntime{wakeRuntime: s.runtimeSeam(), noTurnFeed: s.replyNoTurnFeed, turnBusy: func(sessionID string) bool {
		state, ok := s.SessionTurnOutputState(sessionID)
		if !ok {
			return false
		}
		turnID, _ := state.CurrentTurn()
		return turnID != ""
	}}
}

func (s *Service) publishReplyEvent(kind string, ev events.RoutingReplyEvent) {
	if s.Bus == nil {
		return
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	if err := s.Bus.Publish(context.Background(), events.Event{Scope: events.ScopeSession, SessionID: ev.TargetSessionID,
		LogicalAgentID: ev.LogicalAgentID, Kind: kind, PayloadJSON: string(data)}); err != nil {
		log.Printf("routing reply %s: publish %s: %v", ev.ReplyID, kind, err)
	}
}

// SubmitRoutingReply accepts a reply to a routed message and queues it for the
// session that sent that message. See the file comment.
func (s *Service) SubmitRoutingReply(ctx context.Context, req api.RoutingReplyRequest) (api.RoutingReplyReceipt, error) {
	d := s.replies.Load()
	if d == nil {
		return api.RoutingReplyReceipt{}, api.ErrRoutingRepliesNotWired
	}
	if strings.TrimSpace(req.Body) == "" {
		return api.RoutingReplyReceipt{}, fmt.Errorf("%w: body is required", api.ErrReplyInvalid)
	}
	if len(req.Body) > replyMaxBodyBytes {
		return api.RoutingReplyReceipt{}, fmt.Errorf("%w: %d bytes, limit %d", api.ErrReplyTooLarge, len(req.Body), replyMaxBodyBytes)
	}
	if !utf8.ValidString(req.Body) {
		return api.RoutingReplyReceipt{}, fmt.Errorf("%w: body must be valid UTF-8", api.ErrReplyInvalid)
	}
	caller, err := messaging.ParseURN(req.Caller.ID)
	if err != nil {
		return api.RoutingReplyReceipt{}, fmt.Errorf("%w: caller identity: %w", api.ErrReplyInvalid, err)
	}

	parent, err := s.Store.MessagingStore().Get(ctx, req.ParentID)
	if errors.Is(err, messaging.ErrNotFound) {
		return api.RoutingReplyReceipt{}, api.ErrReplyParentNotFound
	}
	if err != nil {
		return api.RoutingReplyReceipt{}, err
	}
	target, err := replyTargetSession(parent)
	if err != nil {
		return api.RoutingReplyReceipt{}, err
	}
	if s.ReplyAuthorization != nil {
		if err := s.ReplyAuthorization(ctx, req.Caller, parent, target); err != nil {
			return api.RoutingReplyReceipt{}, fmt.Errorf("%w: %w", api.ErrReplyForbidden, err)
		}
	} else if !req.Verified {
		log.Printf("routing reply: unverified caller %q replying to %s (observe mode)", req.Caller.ID, parent.ID)
	}

	// A running PTY has no turn lifecycle: refuse rather than queue what could
	// never be delivered. (A session that has ended is accepted; its replies hand
	// off, and the dispatcher re-checks the successor.)
	if d.rt.noTurnFeed != nil && d.rt.noTurnFeed(target) {
		return api.RoutingReplyReceipt{}, api.ErrReplyNoTurnFeed
	}

	logicalAgent := parent.Metadata["logical_agent_id"]
	if row, err := s.Store.GetSession(target); err == nil && row != nil && row.LogicalAgentID != "" {
		logicalAgent = row.LogicalAgentID
	}
	in := store.NewRoutingReply{From: caller, ParentID: parent.ID, ThreadID: parent.ThreadID, Body: req.Body,
		TargetSessionID: target, LogicalAgentID: logicalAgent, Actor: caller.URN(), Interrupt: req.Interrupt, IdempotencyKey: req.IdempotencyKey}

	if !req.Interrupt {
		reply, created, err := s.Store.CreateRoutingReply(ctx, in)
		if err != nil {
			return api.RoutingReplyReceipt{}, replyStoreError(err)
		}
		if created {
			d.notifyNew(target)
		}
		return replyReceipt(reply, "", !created), nil
	}

	// An interrupting reply is reserved first, as a pending row that carries its
	// idempotency key and its priority, then the running turn is canceled, then
	// the reply is made deliverable. Reserving first means a retry (including one
	// after the client disconnected mid-cancel) finds the reply instead of
	// canceling again, which could hit a turn an older reply has since started.
	// The session is held for the whole window: the cancel's idle boundary must
	// not hand the next turn to an older queued reply, because an interrupting
	// reply is the session's next turn (ADR 0049 s1.6) and is ordered ahead of
	// every non-interrupting one.
	release := d.lockSubmit(target)
	defer release()
	reserved := in
	reserved.Pending = true
	reply, created, err := s.Store.CreateRoutingReply(ctx, reserved)
	if err != nil {
		return api.RoutingReplyReceipt{}, replyStoreError(err)
	}
	if !created {
		return replyReceipt(reply, "", true), nil
	}
	outcome, err := s.interruptForReply(ctx, target, caller.URN())
	// Finish on a context the client cannot cancel: once the turn is canceled the
	// reply must not be left half-made because the connection dropped.
	finish := context.WithoutCancel(ctx)
	if err != nil {
		// Refused or failed: the reply was never accepted, so it leaves no trace.
		if discardErr := s.Store.DiscardPendingRoutingReply(finish, reply.ReplyID); discardErr != nil {
			log.Printf("routing reply %s: discard after refused interrupt: %v", reply.ReplyID, discardErr)
		}
		return api.RoutingReplyReceipt{}, err
	}
	if err := s.Store.PromoteRoutingReply(finish, reply.ReplyID); err != nil {
		return api.RoutingReplyReceipt{}, err
	}
	reply.State = store.RoutingReplyQueued
	return replyReceipt(reply, outcome, false), nil
}

// replyTargetSession is the session that sent the routed message: its sender
// URN, cross-checked against the session_id the staged output recorded.
func replyTargetSession(parent messaging.Envelope) (string, error) {
	// Only a message a session published to a channel is "routed". A mailbox
	// message that happens to carry a session sender is answered through the
	// mailbox, exactly as POST /messages with in_reply_to treats it.
	if _, ok := channels.AddressName(parent.To); !ok {
		return "", fmt.Errorf("%w: it was not published to a channel", api.ErrReplyTargetNotSession)
	}
	if parent.From.Kind != messaging.KindSession || parent.From.Authority != "local" || parent.From.ID == "" {
		return "", api.ErrReplyTargetNotSession
	}
	if recorded := parent.Metadata["session_id"]; recorded != "" && recorded != parent.From.ID {
		return "", fmt.Errorf("%w: sender %s but the message records session %s", api.ErrReplyTargetNotSession, parent.From.URN(), recorded)
	}
	return parent.From.ID, nil
}

// interruptForReply cancels the session's open turn and waits for it to end.
// A session with no turn to cancel is not an error: the reply is a plain next
// turn. A runtime that cannot cancel, or a turn that has not started, refuses
// the reply (ADR 0049 s1.6).
func (s *Service) interruptForReply(ctx context.Context, sessionID, actor string) (string, error) {
	if s.interrupter == nil {
		return "", api.ErrReplyInterruptUnsupported
	}
	_, err := s.interrupter.CancelTurnAndWait(ctx, sessionID, actor)
	var refusal *TurnInterruptRefusal
	switch {
	case err == nil:
		return replyInterruptCancelled, nil
	// The session ended: even a typed refusal that also wraps this sentinel (an
	// exit-flushed terminal) means the reply follows the ended-session path.
	case errors.Is(err, agentsessions.ErrSessionNotRunning):
		return replyInterruptNotRunning, nil
	case errors.As(err, &refusal):
		switch refusal.Reason {
		case TurnInterruptNoTurn:
			return replyInterruptNoTurn, nil
		case TurnInterruptSuperseded:
			return replyInterruptSuperseded, nil
		case TurnInterruptNotStarted:
			return "", api.ErrReplyTurnNotStarted
		case TurnInterruptUnsupported:
			return "", api.ErrReplyInterruptUnsupported
		case TurnInterruptTimeout:
			return replyInterruptTimedOut, nil
		case TurnInterruptSessionEnded:
			return replyInterruptNotRunning, nil // wraps ErrSessionNotRunning; matched above
		}
		return "", err
	case errors.Is(err, agentsessions.ErrInterruptUnsupported):
		return "", api.ErrReplyInterruptUnsupported
	}
	return "", err
}

func replyStoreError(err error) error {
	if errors.Is(err, store.ErrRoutingReplyIdempotencyConflict) {
		return api.ErrReplyIdempotencyConflict
	}
	return err
}

func replyReceipt(r store.RoutingReply, interrupt string, duplicate bool) api.RoutingReplyReceipt {
	return api.RoutingReplyReceipt{ReplyID: r.ReplyID, ParentID: r.ParentID, State: string(r.State),
		TargetSessionID: r.TargetSessionID, Interrupt: interrupt, Duplicate: duplicate}
}

// RoutingReplyDelivery is what consumers read to learn where a reply ended up.
func (s *Service) RoutingReplyDelivery(ctx context.Context, replyID string) (api.RoutingReplyDelivery, error) {
	r, err := s.Store.RoutingReply(ctx, replyID)
	if errors.Is(err, store.ErrRoutingReplyNotFound) {
		return api.RoutingReplyDelivery{}, api.ErrReplyNotFound
	}
	if err != nil {
		return api.RoutingReplyDelivery{}, err
	}
	return api.RoutingReplyDelivery{ReplyID: r.ReplyID, ParentID: r.ParentID, State: string(r.State), Reason: r.Reason,
		Detail: r.Detail, OriginalSessionID: r.OriginalSessionID, TargetSessionID: r.TargetSessionID,
		DeliveredToSessionID: r.DeliveredToSessionID, InterruptRequested: r.Interrupt, Attempts: r.Attempts,
		NextAttemptAt: r.NextAttemptAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, SettledAt: r.SettledAt}, nil
}

// replyNoTurnFeed reports a running session whose runtime has no turn lifecycle.
// A PTY's marker is set when input is written but nothing ever clears it, so
// Tether cannot tell when such a session is idle and refuses to queue replies
// for it rather than deliver one and strand the rest.
func (s *Service) replyNoTurnFeed(sessionID string) bool {
	if s.Manager == nil {
		return false
	}
	info, ok := s.Manager.Get(sessionID)
	return ok && info.Caps.PTY
}
