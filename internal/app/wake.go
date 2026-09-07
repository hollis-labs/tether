package app

// wake.go — T06 (messaging vNext, CW-20260906-0037): durable hosted-session
// handoff, route fencing and capability-based steering.
//
// Before this file: resolveNotifySession's msg://agent/... fallback (T05,
// internal/api/messages.go) picked "the newest running session with a
// matching LogicalAgentID" via a raw ListSessions(state=running) scan --
// exactly the "newest-running-session lookup" the architecture calls out to
// replace. And T03's delivery_store.go said outright: "nothing yet reads
// FROM the delivery core as a primary path (that's T06's pump)" -- Consume
// was the only caller of Claim/Ack, and it stamped host_accepted/
// turn_submitted/consumed together, synthetically, at consume time, not at
// the moment a host actually accepted/submitted the turn.
//
// This file makes both real:
//   - ResolveActorSession consults the T02 RuntimeBinding primitive
//     (CurrentBinding) as the authoritative "who owns delivery for this
//     actor right now" answer. A bound owner that isn't currently running
//     is an honest "offline" -- it never silently falls back to scanning
//     for a different running session (that would be exactly the kind of
//     unauthorized reroute of a stable-actor destination the architecture
//     forbids: "stable actor destinations follow authorized activation").
//     The legacy newest-running-session scan runs only when no binding has
//     ever been leased for the target, or every prior lapsed binding was
//     non-pull-only -- a pull-only binding's "never wake-targetable" fence
//     survives its own lease expiry, so an unrenewed published-local
//     bridge is never silently rerouted to an unrelated running session.
//   - AttemptWake drives the actual go-messaging delivery.Store Claim/Ack/
//     Nack sequence around a wake attempt: Claim, Ack(host_accepted) the
//     moment Tether is about to hand off to a concrete session, then check
//     for a stale-generation race and busy/offline before ever calling
//     SendTurn, and Ack(turn_submitted) only once SendTurn actually
//     succeeds. A busy session or a submit failure Nacks the delivery
//     retryable (bounded backoff) instead of promising something that
//     didn't happen or trying a different session for the same actor.
//   - RunWakeSweep is the "shared pump": a bounded, non-blocking retry pass
//     over deliveries the delivery core already knows are ready (pending or
//     past their retry backoff), so a busy/offline wake attempt is not lost
//     -- it is retried later using the exact same AttemptWake path a fresh
//     notify call uses, with no separate bespoke queue.
//
// Runtime interaction (RuntimeHealth/SendTurn) is threaded through the
// small wakeRuntime seam rather than called on *Service directly inside the
// unexported implementation functions. This is what let wake_test.go drive
// the FULL delivery-core/registry-binding logic below against a real
// *store.Store and *registry.Service (real SQLite, real generation
// fencing, real receipt stages) while faking only the OS-process runtime
// layer that agentkit itself already owns and compliance-tests (see
// service_claude_code_test.go) -- Tether has no business re-testing that
// layer, only its own consumption of it.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"
	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/delivery"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

const (
	// wakeClaimLeaseDuration mirrors delivery_store.go's
	// deliveryConsumeLeaseDuration: long enough to cover one immediate
	// Claim-then-Ack/Nack sequence, short enough that a crash mid-attempt
	// doesn't tie up the obligation for long before it's reclaimable.
	wakeClaimLeaseDuration = 30 * time.Second
	// wakeBusyRetryBackoff / wakeOfflineRetryBackoff bound how soon a
	// Nacked wake attempt becomes claimable again -- short for "try again
	// once the current turn probably finished," longer for "nothing
	// observable changed, no need to hammer it."
	wakeBusyRetryBackoff    = 5 * time.Second
	wakeOfflineRetryBackoff = 30 * time.Second
	// wakeSweepBatchLimit caps one RunWakeSweep pass so a large backlog
	// cannot make a single sweep tick run unboundedly long. Uncapped
	// remainder is picked up by the next tick, not dropped.
	wakeSweepBatchLimit = 50
)

// wakeRuntime is the OS-process-layer seam AttemptWake/ResolveActorSession
// need: "is this session alive/idle/busy" and "hand it a turn." Production
// callers get this from (*Service).runtimeSeam(); wake_test.go supplies a
// deterministic fake so the delivery-core/registry-binding logic below can
// be driven end-to-end without spawning a real provider subprocess.
type wakeRuntime struct {
	health   func(sessionID string) (api.RuntimeHealthResult, bool)
	sendTurn func(ctx context.Context, sessionID, text string) error
}

func (s *Service) runtimeSeam() wakeRuntime {
	return wakeRuntime{
		health:   s.RuntimeHealth,
		sendTurn: s.SendTurn,
	}
}

// ResolveActorSession answers "which session currently owns delivery for
// this logical agent" -- see the file doc comment for the binding-first,
// legacy-fallback contract.
func (s *Service) ResolveActorSession(ctx context.Context, logicalAgentID string) (string, error) {
	return resolveActorSession(ctx, s.Store, s.Registry, s.runtimeSeam(), logicalAgentID)
}

// AttemptWake drives one Claim/Ack/Nack wake attempt for messageID/to
// against the already-resolved sessionID. See the file doc comment.
func (s *Service) AttemptWake(ctx context.Context, messageID string, to messaging.Address, sessionID, wakeText string) api.WakeOutcome {
	return attemptWake(ctx, s.Store, s.Registry, s.runtimeSeam(), messageID, to, sessionID, wakeText)
}

// RunWakeSweep is the shared pump's one bounded, non-blocking pass. See
// the file doc comment.
func (s *Service) RunWakeSweep(ctx context.Context) (int, error) {
	return runWakeSweep(ctx, s.Store, s.Registry, s.runtimeSeam())
}

func resolveActorSession(ctx context.Context, st *store.Store, reg *registry.Service, rt wakeRuntime, logicalAgentID string) (string, error) {
	if reg != nil {
		target := registry.LogicalAgentBindingTarget(logicalAgentID)
		b, err := reg.CurrentBinding(ctx, target)
		if err == nil {
			// T07 (messaging vNext): a published-local bridge (leased over
			// HTTP, see internal/api/bindings.go) is never wake-targetable
			// -- it has no Tether-managed session for SendTurn to reach,
			// and Tether never assumes launch/resume authority over one.
			// Reported identically to "bound owner not running": an
			// honest, permanent no-route, never a fallback scan.
			if isPullOnly(b) {
				return "", nil
			}
			if _, ok := rt.health(b.SessionID); ok {
				return b.SessionID, nil
			}
			// Bound owner isn't running: honest offline. Never fall
			// through to a different running session for this actor --
			// that would silently redirect a stable-actor destination
			// without authorized (re-)activation.
			return "", nil
		}
		if !errors.Is(err, registry.ErrBindingNotFound) {
			return "", fmt.Errorf("resolve actor session: current binding: %w", err)
		}
		// CurrentBinding's ErrBindingNotFound is ambiguous by itself: it
		// covers "never leased," "every binding revoked," AND "every
		// binding's lease expired" alike (see its own doc comment in
		// bindings.go). Only the first case is safe to treat as "fall
		// through to the legacy compatibility heuristic" -- a lapsed lease
		// on a binding that was pull-only must still fence exactly like a
		// live one: a published-local bridge's owner must never be
		// silently substituted by an unrelated same-logical-agent-ID
		// session just because its lease wasn't renewed in time (a
		// realistic case: a bridge process misses a renewal window).
		//
		// The check below looks ONLY at the single highest-generation
		// binding (ListBindingsForTarget's own contract: "newest generation
		// first"), not the whole history -- an earlier draft of this fix
		// scanned every prior binding and got this wrong: a target that was
		// briefly bridge-bound, then legitimately reclaimed by Tether's own
		// internal launch path (session_lifecycle.go's leaseActorBinding,
		// which supersedes with a fresh, non-pull-only generation with no
		// visibility guard -- see api/bindings.go's comment on why that
		// path is exempt), and whose Tether-hosted session LATER stopped
		// and revoked its own binding, must fall through to the legacy
		// heuristic like any other ordinary stopped session -- not be
		// permanently fenced forever by a bridge binding several
		// generations back that is no longer the actor's most recent
		// owner of record. Only the MOST RECENT generation's visibility
		// answers "is this actor currently a published-local bridge,"
		// exactly the same generation-fencing rule CurrentBinding itself
		// uses for the live case.
		ever, listErr := reg.ListBindingsForTarget(ctx, target)
		if listErr != nil {
			return "", fmt.Errorf("resolve actor session: list bindings for fallback check: %w", listErr)
		}
		if len(ever) > 0 && isPullOnly(ever[0]) {
			return "", nil
		}
		// Never explicitly bound, or the most recent binding was lapsed
		// non-pull-only (e.g. a stopped Tether-hosted session that revoked
		// or never renewed): fall through to the legacy heuristic. This is
		// the same compatibility behavior a never-bound actor already
		// gets, not a bypass of an active fencing guarantee.
	}
	return legacyNewestRunningSession(st, rt, logicalAgentID)
}

// isPullOnly reports whether b is a published-local bridge binding that
// must never receive a push wake attempt (T07).
func isPullOnly(b registry.RuntimeBinding) bool {
	for _, c := range b.Capabilities {
		if c == api.PullOnlyCapability {
			return true
		}
	}
	return false
}

// legacyNewestRunningSession is resolveNotifySession's pre-T06 fallback,
// preserved verbatim as the documented compatibility path for actors that
// have never had a binding leased. Not the authoritative answer once a
// binding exists -- see resolveActorSession.
func legacyNewestRunningSession(st *store.Store, rt wakeRuntime, logicalAgentID string) (string, error) {
	rows, err := st.ListSessions(store.ListSessionsOptions{State: "running", Limit: 1000})
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if row.LogicalAgentID == logicalAgentID {
			if _, ok := rt.health(row.ID); ok {
				return row.ID, nil
			}
		}
	}
	return "", nil
}

func attemptWake(ctx context.Context, st *store.Store, reg *registry.Service, rt wakeRuntime, messageID string, to messaging.Address, sessionID, wakeText string) api.WakeOutcome {
	if sessionID == "" {
		return api.WakeOutcome{Reason: "offline"}
	}
	if st == nil {
		return sendTurnDirect(ctx, rt, sessionID, wakeText)
	}

	deliveryID, ok, err := st.DeliveryIDForMessage(ctx, messageID)
	if err != nil {
		log.Printf("app: attempt wake: delivery id lookup for message %s failed (falling back to untracked wake): %v", messageID, err)
	}
	if err != nil || !ok {
		// Pre-T03 legacy message (no delivery-core tracking) or a lookup
		// failure: fall back to a direct, untracked wake rather than
		// blocking the caller's notify request on a bookkeeping gap.
		return sendTurnDirect(ctx, rt, sessionID, wakeText)
	}

	ds := st.DeliveryStore()
	claim, claimErr := ds.Claim(ctx, delivery.ClaimRequest{
		DeliveryID:    delivery.DeliveryID(deliveryID),
		Holder:        sessionID,
		LeaseDuration: wakeClaimLeaseDuration,
		Nowait:        true,
	})
	if claimErr != nil {
		// Already claimed by a concurrent attempt (another notify call or
		// the sweep), already terminal, or a genuine delivery-core error:
		// in every case a second wake attempt here risks double-submitting
		// the same turn, so this is a no-op, not a retry-worthy failure.
		return api.WakeOutcome{Reason: "claim-unavailable", Detail: claimErr.Error()}
	}
	lease := delivery.LeaseRef{
		DeliveryID:        claim.Attempt.DeliveryID,
		AttemptID:         claim.Attempt.ID,
		LeaseToken:        claim.Attempt.LeaseToken,
		BindingGeneration: claim.Attempt.BindingGeneration,
	}

	// Real, timely host-accepted receipt: recorded now, at the moment
	// Tether is about to hand this delivery to a concrete session -- not
	// synthesized later at Consume time the way the consume-only path
	// (delivery_store.go's recordConsumedReceipts) bundles all three
	// stages together after the fact.
	if _, _, err := ds.Ack(ctx, delivery.AckRequest{Lease: lease, Stage: delivery.StageHostAccepted}); err != nil {
		log.Printf("app: attempt wake: ack host_accepted for delivery %s failed (best-effort receipt recording skipped): %v", deliveryID, err)
	}

	// Stale-generation race: has ownership of this actor moved to a
	// different session since sessionID was resolved a moment ago? Only
	// meaningful for actor-kind targets -- an exact session address is
	// pinned and never subject to actor rebinding. This narrows, but does
	// not eliminate, the window: a rebind landing between this check and
	// the SendTurn call below is a known, accepted residual race (it does
	// not violate "never spawn a second owner" -- this call still only
	// ever submits to the ORIGINALLY resolved sessionID -- but the check
	// alone cannot guarantee ownership hasn't moved again in that instant;
	// the losing side's own future resolution/sweep still self-corrects).
	if to.Kind == messaging.KindAgent && reg != nil {
		current, err := reg.CurrentBinding(ctx, registry.LogicalAgentBindingTarget(to.ID))
		switch {
		case err == nil && current.SessionID != sessionID:
			nackRetryable(ctx, ds, lease, "stale generation: a newer binding now owns this actor", wakeOfflineRetryBackoff)
			return api.WakeOutcome{Reason: "stale-generation", SessionID: sessionID}
		case err != nil && !errors.Is(err, registry.ErrBindingNotFound):
			// A transient lookup failure must not silently skip the
			// fencing check and proceed as if ownership were confirmed
			// current -- log it so a real DB problem here is visible,
			// even though the wake attempt still proceeds (the binding
			// existed at resolution time; treating a lookup hiccup as an
			// automatic abort would make wakes needlessly fragile).
			log.Printf("app: attempt wake: stale-generation check for %s failed (proceeding with the already-resolved session): %v", to.URN(), err)
		}
	}

	health, ok := rt.health(sessionID)
	if !ok {
		nackRetryable(ctx, ds, lease, "session not running", wakeOfflineRetryBackoff)
		return api.WakeOutcome{Reason: "offline-race", SessionID: sessionID}
	}
	if health.Health.State == agentsessions.LiveStateProcessing {
		nackRetryable(ctx, ds, lease, "session busy", wakeBusyRetryBackoff)
		return api.WakeOutcome{Reason: "busy", SessionID: sessionID}
	}

	if err := rt.sendTurn(ctx, sessionID, wakeText); err != nil {
		// Unsupported steering / turn-submit failure fails honestly: the
		// delivery is released for retry, but this call never falls back
		// to a different session for the same actor -- a failed selected
		// host must never silently spawn a second local owner.
		nackRetryable(ctx, ds, lease, err.Error(), wakeBusyRetryBackoff)
		return api.WakeOutcome{Attempted: true, SessionID: sessionID, Reason: "turn-submit-failed", Detail: err.Error()}
	}

	if _, _, err := ds.Ack(ctx, delivery.AckRequest{Lease: lease, Stage: delivery.StageTurnSubmitted}); err != nil {
		log.Printf("app: attempt wake: ack turn_submitted for delivery %s failed (best-effort receipt recording skipped): %v", deliveryID, err)
	}
	return api.WakeOutcome{Attempted: true, Delivered: true, SessionID: sessionID}
}

func sendTurnDirect(ctx context.Context, rt wakeRuntime, sessionID, wakeText string) api.WakeOutcome {
	if err := rt.sendTurn(ctx, sessionID, wakeText); err != nil {
		return api.WakeOutcome{Attempted: true, SessionID: sessionID, Reason: "turn-submit-failed", Detail: err.Error()}
	}
	return api.WakeOutcome{Attempted: true, Delivered: true, SessionID: sessionID}
}

func nackRetryable(ctx context.Context, ds delivery.Store, lease delivery.LeaseRef, reason string, backoff time.Duration) {
	if _, _, err := ds.Nack(ctx, delivery.NackRequest{
		Lease:         lease,
		Retryable:     true,
		Error:         reason,
		NextAttemptAt: time.Now().Add(backoff),
	}); err != nil {
		log.Printf("app: attempt wake: nack delivery %s (%s) failed: %v", lease.DeliveryID, reason, err)
	}
}

// resolveWakeTarget dispatches by recipient kind for the sweep, which has
// no per-call explicit session_id override the way notify does -- it only
// ever works from the delivery's own recorded recipient address.
func resolveWakeTarget(ctx context.Context, st *store.Store, reg *registry.Service, rt wakeRuntime, to messaging.Address) (string, error) {
	switch to.Kind {
	case messaging.KindSession:
		if _, ok := rt.health(to.ID); ok {
			return to.ID, nil
		}
		return "", nil
	case messaging.KindAgent:
		return resolveActorSession(ctx, st, reg, rt, to.ID)
	default:
		return "", nil
	}
}

func runWakeSweep(ctx context.Context, st *store.Store, reg *registry.Service, rt wakeRuntime) (int, error) {
	if st == nil {
		return 0, nil
	}
	ds := st.DeliveryStore()
	ready, err := ds.ListDeliveries(ctx, delivery.Filter{
		Status:    []delivery.DeliveryStatus{delivery.DeliveryPending, delivery.DeliveryRetryScheduled},
		ReadyOnly: true,
		Limit:     wakeSweepBatchLimit,
	})
	if err != nil {
		return 0, fmt.Errorf("run wake sweep: list deliveries: %w", err)
	}

	attempted := 0
	for _, rd := range ready {
		if ctx.Err() != nil {
			break
		}
		sessionID, resolveErr := resolveWakeTarget(ctx, st, reg, rt, rd.Recipient)
		if resolveErr != nil {
			log.Printf("app: wake sweep: resolve session for %s failed: %v", rd.Recipient.URN(), resolveErr)
			continue
		}
		if sessionID == "" {
			continue // still offline; a later sweep will retry
		}
		msg, err := st.MessagingStore().Get(ctx, string(rd.MessageID))
		if err != nil {
			log.Printf("app: wake sweep: get message %s failed: %v", rd.MessageID, err)
			continue
		}
		outcome := attemptWake(ctx, st, reg, rt, string(rd.MessageID), rd.Recipient, sessionID, sweepWakeText(msg))
		if outcome.Attempted {
			attempted++
		}
	}
	if len(ready) == wakeSweepBatchLimit {
		log.Printf("app: wake sweep: batch limit %d reached; more ready deliveries may remain for the next sweep", wakeSweepBatchLimit)
	}
	return attempted, nil
}

// sweepWakeText reconstructs a mailbox-wake notification for a retried
// delivery. The sweep has no access to the original notify call's
// req.WakeText override (that is a per-call parameter, never persisted),
// so every retried wake uses this generated form -- consistent with
// mailboxWakeText's shape in internal/api/messages.go, duplicated rather
// than shared to avoid an internal/app -> internal/api dependency for one
// formatting helper (internal/app is already depended on BY internal/api's
// production wiring, not the reverse).
func sweepWakeText(env messaging.Envelope) string {
	urgency := env.Metadata["urgency"]
	if urgency == "" {
		urgency = "normal"
	}
	return fmt.Sprintf("**Mailbox wake (daemon-injected, retried)** — you have unread message(s) waiting. Latest message: `%s` from `%s` to `%s`, kind `%s`, urgency `%s`.\n\nPlease run your inbox-check procedure, process messages normally, and mark handled messages read or consumed according to your checklist. This is a delivery notification, not an instruction to abandon the current task unless your own procedure says the message is urgent.",
		env.ID, env.From.URN(), env.To.URN(), env.Kind, urgency)
}
