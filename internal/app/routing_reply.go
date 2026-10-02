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
	messaging "github.com/hollis-labs/go-messaging"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
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

// Interrupt outcomes reported on the receipt.
const (
	replyInterruptCancelled  = "cancelled"
	replyInterruptNoTurn     = "no_turn_in_progress"
	replyInterruptSuperseded = "turn_superseded"
	replyInterruptNotRunning = "session_not_running"
)

// ReplyAuthorization is the caller-identity hook for replies, shaped like the
// channel Authorization. Nil is observe mode (ADR 0045): the verified principal
// (else the self-asserted ?as=) is recorded as the actor and nothing is
// refused. CW-20260930-0253 phases 2-3 install a checker here.
type ReplyAuthorization func(ctx context.Context, caller identity.Principal, parent messaging.Envelope, targetSessionID string) error

// turnInterrupter is the seam to CW-20261002-0067's Service.CancelTurnAndWait.
type turnInterrupter interface {
	CancelTurnAndWait(ctx context.Context, sessionID, actor string) (TurnInterruptResult, error)
}

// replyRuntime is the process-layer seam: wakeRuntime's health and sendTurn
// plus whether Tether's own marker says a turn is in flight.
type replyRuntime struct {
	wakeRuntime
	turnBusy func(sessionID string) bool
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
}

func newReplyDispatcher(ctx context.Context, st *store.Store, reg *registry.Service, rt replyRuntime, pub func(string, events.RoutingReplyEvent)) *replyDispatcher {
	ctx, cancel := context.WithCancel(ctx)
	return &replyDispatcher{st: st, reg: reg, rt: rt, pub: pub, ctx: ctx, cancel: cancel, sessions: map[string]*replyDrain{}}
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
	if st.running {
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
// called.
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
		if st.submitters == 0 && !st.running && !st.again {
			delete(d.sessions, sessionID)
		}
		d.mu.Unlock()
	}
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
func (d *replyDispatcher) drain(sessionID string) bool {
	for d.ctx.Err() == nil {
		rows, err := d.st.QueuedRoutingReplies(d.ctx, sessionID, time.Now(), replyDrainBatch)
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
		if d.busy(sessionID) {
			return false // the turn's completion notifies again
		}
		r := rows[0]
		claimed, err := d.st.ClaimRoutingReply(d.ctx, r.ReplyID)
		if err != nil {
			log.Printf("routing reply %s: claim: %v", r.ReplyID, err)
			return false
		}
		if !claimed {
			continue // another drain took it; look at the next
		}
		d.deliver(sessionID, r)
		return true
	}
	return false
}

// deliver injects r's body as sessionID's next turn. r was claimed, so r.Attempts
// is stale by one: the stored attempt count is authoritative.
func (d *replyDispatcher) deliver(sessionID string, r store.RoutingReply) {
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

// sweep drains every session that has a queued reply. It is the repair path for
// runtimes with no turn feed and for rows whose retry time has arrived.
func (d *replyDispatcher) sweep(ctx context.Context) (int, error) {
	rows, err := d.st.RoutingRepliesInState(ctx, store.RoutingReplyQueued, 500)
	if err != nil {
		return 0, err
	}
	seen := map[string]struct{}{}
	for _, r := range rows {
		if _, dup := seen[r.TargetSessionID]; dup {
			continue
		}
		seen[r.TargetSessionID] = struct{}{}
		d.notify(r.TargetSessionID)
	}
	return len(seen), nil
}

// recover resolves rows a previous process left 'delivering'. Whether the
// runtime took the turn is unknown, so a reply whose session is still running
// is NOT sent again (at-most-once per session): it becomes undeliverable with
// ReplyReasonRestart. If the session is gone the row goes back to the queue and
// follows the ended-session path to a bound successor.
func (d *replyDispatcher) recover(ctx context.Context) error {
	rows, err := d.st.RoutingRepliesInState(ctx, store.RoutingReplyDelivering, 1000)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if _, running := d.rt.health(r.TargetSessionID); running {
			d.settle(r, store.RoutingReplySettlement{State: store.RoutingReplyUndeliverable, Reason: ReplyReasonRestart,
				Detail: "the daemon restarted while this reply was being injected; it may or may not have reached the session, so it was not sent again"})
			continue
		}
		d.requeue(r, store.RoutingReplyRequeue{Reason: ReplyReasonRestart, Detail: "requeued after a daemon restart; the session it was being injected into is gone"})
	}
	_, err = d.sweep(ctx)
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
	d := newReplyDispatcher(ctx, s.Store, s.Registry, s.replyRuntime(), s.publishReplyEvent)
	if !s.replies.CompareAndSwap(nil, d) {
		d.stop()
		return nil
	}
	if s.interrupter == nil {
		// Service.CancelTurnAndWait lands with CW-20261002-0067; until it is in
		// this tree the assertion fails and interrupt:true is refused as unsupported.
		if ti, ok := any(s).(turnInterrupter); ok {
			s.interrupter = ti
		}
	}
	return d.recover(ctx)
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
	return replyRuntime{wakeRuntime: s.runtimeSeam(), turnBusy: func(sessionID string) bool {
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
		return api.RoutingReplyReceipt{}, fmt.Errorf("%w: caller identity: %v", api.ErrReplyInvalid, err)
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
			return api.RoutingReplyReceipt{}, fmt.Errorf("%w: %v", api.ErrReplyForbidden, err)
		}
	} else if !req.Verified {
		log.Printf("routing reply: unverified caller %q replying to %s (observe mode)", req.Caller.ID, parent.ID)
	}

	logicalAgent := parent.Metadata["logical_agent_id"]
	if row, err := s.Store.GetSession(target); err == nil && row != nil && row.LogicalAgentID != "" {
		logicalAgent = row.LogicalAgentID
	}
	in := store.NewRoutingReply{From: caller, ParentID: parent.ID, ThreadID: parent.ThreadID, Body: req.Body,
		TargetSessionID: target, LogicalAgentID: logicalAgent, Actor: caller.URN(), Interrupt: req.Interrupt, IdempotencyKey: req.IdempotencyKey}

	outcome := ""
	if req.Interrupt {
		// Serialized per session, so the idempotency check below happens before
		// any cancel is sent and a retry cannot cancel its twin's turn.
		release := d.lockSubmit(target)
		defer release()
		if prior, found, err := s.Store.PeekRoutingReply(ctx, in); err != nil {
			return api.RoutingReplyReceipt{}, replyStoreError(err)
		} else if found {
			return replyReceipt(prior, "", true), nil
		}
		if outcome, err = s.interruptForReply(ctx, target, caller.URN()); err != nil {
			return api.RoutingReplyReceipt{}, err
		}
	}

	reply, created, err := s.Store.CreateRoutingReply(ctx, in)
	if err != nil {
		return api.RoutingReplyReceipt{}, replyStoreError(err)
	}
	if created {
		d.notifyNew(target)
	}
	return replyReceipt(reply, outcome, !created), nil
}

// replyTargetSession is the session that sent the routed message: its sender
// URN, cross-checked against the session_id the staged output recorded.
func replyTargetSession(parent messaging.Envelope) (string, error) {
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
		}
		return "", err
	case errors.Is(err, agentsessions.ErrInterruptUnsupported):
		return "", api.ErrReplyInterruptUnsupported
	case errors.Is(err, agentsessions.ErrSessionNotRunning):
		return replyInterruptNotRunning, nil
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
