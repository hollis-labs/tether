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
//     succeeds, then holds the lease for wakeConsumeWindow so the
//     recipient's consumption, not lease expiry, ends the attempt. A
//     busy session or a submit failure Nacks the delivery
//     retryable (bounded backoff) instead of promising something that
//     didn't happen or trying a different session for the same actor.
//   - RunWakeSweep is the "shared pump": a bounded, non-blocking retry pass
//     over deliveries the delivery core already knows are ready (pending or
//     past their retry backoff), so a busy/offline wake attempt is not lost
//     -- it is retried later using the exact same AttemptWake path a fresh
//     notify call uses, with no separate bespoke queue. A delivery whose
//     recipient has no live session is parked in memory with backoff
//     (wakeParkSet) instead of being re-resolved every tick.
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
	"sync"
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
	// wakeConsumeWindow is how long a wake whose turn was submitted keeps
	// its lease, awaiting the recipient's consumption (CW-20261001-0016).
	// A submitted turn is not consumption -- the wake text only tells the
	// agent to check its inbox -- so the attempt stays leased at
	// turn_submitted and Consume (or the recipient's own inbox pull)
	// closes it to delivered through the pending-receipt marker. Left at
	// wakeClaimLeaseDuration, the lease lapsed long before an agent's turn
	// reached its inbox, and every successful wake fell into
	// retry_scheduled. A wake nobody consumes within the window lapses into
	// one retry, which re-wakes the recipient as a reminder. The lease also
	// holds the delivery against other claimants: a T07 bridge claim gets
	// ErrAlreadyClaimed for up to this long.
	wakeConsumeWindow = 15 * time.Minute
	// wakeBusyRetryBackoff / wakeOfflineRetryBackoff bound how soon a
	// Nacked wake attempt becomes claimable again -- short for "try again
	// once the current turn probably finished," longer for "nothing
	// observable changed, no need to hammer it."
	wakeBusyRetryBackoff    = 5 * time.Second
	wakeOfflineRetryBackoff = 30 * time.Second
	// wakeSweepBatchLimit caps one RunWakeSweep pass so a large backlog
	// cannot make a single sweep tick run unboundedly long. Uncapped
	// remainder is picked up by the next tick, not dropped. Parked
	// deliveries (see wakeParkSet) do not count against it.
	wakeSweepBatchLimit = 50
	// wakeParkBaseBackoff / wakeParkMaxBackoff bound how long the sweep
	// leaves a delivery alone after finding no live session for its
	// recipient: the first miss parks it for the base, each consecutive
	// miss doubles that, up to the max. The max is also the longest a
	// recipient that comes back online waits for the sweep to wake it.
	wakeParkBaseBackoff = 30 * time.Second
	wakeParkMaxBackoff  = 2 * time.Minute
	// wakeParkMaxEntries bounds the park set's memory and the extra rows
	// each sweep reads past it. Beyond it, an unresolvable delivery is
	// simply re-checked every sweep, as before parking existed.
	wakeParkMaxEntries = 4096
)

// wakeParkSet is the sweep's memory of deliveries whose recipient had no
// live session the last time it looked (CW-20261001-0012). Without it the
// sweep re-resolved the same unresolvable head rows every tick, and since
// delivery IDs are time-ordered and listed oldest first, a backlog of 50
// such rows starved every newer delivery behind them indefinitely.
//
// Parking is deliberately in memory rather than a Claim+Nack that pushes
// next_attempt_at out: a delivery's next_attempt_at gates every Claim, not
// just the sweep's, so a durable park would also refuse Consume's receipt
// claim and a published-local bridge's own claim for as long as it lasted,
// and each park would write an attempt and receipts for a recipient that
// may never come back. A daemon restart forgets the set, which costs one
// re-check of each parked delivery. The zero value is ready to use.
type wakeParkSet struct {
	mu      sync.Mutex
	entries map[delivery.DeliveryID]wakeParkEntry
}

type wakeParkEntry struct {
	until  time.Time
	misses int
}

func (p *wakeParkSet) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

func (p *wakeParkSet) parked(id delivery.DeliveryID, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[id]
	return ok && now.Before(e.until)
}

// park records one more miss for id and reports whether it is now parked
// (false only when the set is full and id was not already in it). It
// returns the miss count so the caller can tell a first park from a
// repeat one.
func (p *wakeParkSet) park(id delivery.DeliveryID, now time.Time) (misses int, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, exists := p.entries[id]
	if !exists && len(p.entries) >= wakeParkMaxEntries {
		return 0, false
	}
	if p.entries == nil {
		p.entries = map[delivery.DeliveryID]wakeParkEntry{}
	}
	e.misses++
	backoff := wakeParkBaseBackoff
	for i := 1; i < e.misses && backoff < wakeParkMaxBackoff; i++ {
		backoff *= 2
	}
	if backoff > wakeParkMaxBackoff {
		backoff = wakeParkMaxBackoff
	}
	e.until = now.Add(backoff)
	p.entries[id] = e
	return e.misses, true
}

func (p *wakeParkSet) release(id delivery.DeliveryID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, id)
}

// retain drops every entry not in ready. Only call it with a complete
// listing of ready deliveries: an entry missing from a truncated one may
// simply lie past the listing's end.
func (p *wakeParkSet) retain(ready []delivery.RecipientDelivery) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.entries) == 0 {
		return
	}
	keep := make(map[delivery.DeliveryID]struct{}, len(ready))
	for _, rd := range ready {
		keep[rd.ID] = struct{}{}
	}
	for id := range p.entries {
		if _, ok := keep[id]; !ok {
			delete(p.entries, id)
		}
	}
}

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
	return runWakeSweep(ctx, s.Store, s.Registry, s.runtimeSeam(), &s.wakePark)
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
			// without authorized (re-)activation. Said as an error so
			// notify can report why it did not wake (CW-20260912-0134);
			// an ended session's binding is revoked when it exits, so
			// this is a session that died without the daemon seeing it.
			return "", fmt.Errorf("%w: %s", api.ErrBoundSessionNotRunning, b.SessionID)
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
	// Explicit notify overrides and untracked legacy messages must respect
	// the same actor fence as the sweep's binding-first resolution.
	bindingGeneration, blocked := checkWakeBinding(ctx, reg, to, sessionID, 0)
	if blocked.Reason != "" {
		return blocked
	}
	if st == nil {
		return sendTurnDirect(ctx, reg, rt, to, sessionID, wakeText, bindingGeneration)
	}

	deliveryID, ok, err := st.DeliveryIDForMessage(ctx, messageID)
	if err != nil {
		log.Printf("app: attempt wake: delivery id lookup for message %s failed (falling back to untracked wake): %v", messageID, err)
	}
	if err != nil || !ok {
		// Pre-T03 legacy message (no delivery-core tracking) or a lookup
		// failure: fall back to a direct, untracked wake rather than
		// blocking the caller's notify request on a bookkeeping gap.
		return sendTurnDirect(ctx, reg, rt, to, sessionID, wakeText, bindingGeneration)
	}

	// T09 (messaging vNext, CW-20260906-0040): capture the binding
	// generation live at claim time so it lands on the persisted Attempt
	// (delivery.Claim stores whatever BindingGeneration it's given,
	// purely for observability -- it enforces nothing). Before this,
	// Tether never set this field on ANY claim, so a delivery trace could
	// never answer "which binding generation actually served this
	// attempt" -- confirmed by T09 design research: every persisted
	// Attempt had BindingGeneration=0 regardless of which binding was
	// really live. Only meaningful for actor-kind targets, matching the
	// stale-generation check below; an exact session address has no
	// binding generation to report.
	ds := st.DeliveryStore()
	claim, claimErr := ds.Claim(ctx, delivery.ClaimRequest{
		DeliveryID:        delivery.DeliveryID(deliveryID),
		Holder:            sessionID,
		LeaseDuration:     wakeClaimLeaseDuration,
		Nowait:            true,
		BindingGeneration: bindingGeneration,
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

	// The recipient may already have handled this message while the
	// delivery waited out a retry backoff, when Consume could not claim it
	// (CW-20261001-0016). Waking it again would be a duplicate; settle the
	// delivery with the consumption receipt instead. consumed_at and read_at
	// are explicit recipient acts; delivered_at is not consulted, because
	// any caller's inbox listing stamps it.
	if handled, err := st.MessageHandledByRecipient(ctx, messageID); err != nil {
		log.Printf("app: attempt wake: handled check for message %s failed (proceeding with the wake): %v", messageID, err)
	} else if handled {
		if _, _, err := ds.Ack(ctx, delivery.AckRequest{Lease: lease, Stage: delivery.StageConsumed}); err != nil {
			nackRetryable(ctx, ds, lease, "settle already-handled message: "+err.Error(), wakeBusyRetryBackoff)
			return api.WakeOutcome{Reason: "settle-failed", SessionID: sessionID, Detail: err.Error()}
		}
		return api.WakeOutcome{Reason: "already-handled", SessionID: sessionID}
	}

	// Mark this claim as Consume-owned immediately: attemptWake drives the
	// lease through StageTurnSubmitted below and then deliberately leaves
	// it open "awaiting consumption," which only Consume's own receipt
	// recording (internal/store/delivery_store.go's recordConsumedReceipts)
	// is meant to finish. That function only reuses an already-active
	// lease when this exact marker matches it -- see its doc comment for
	// why (an independent review found blindly reusing ANY active lease
	// could hijack an unrelated external claimant's still-in-flight work
	// via the T07 published-local bridge surface).
	//
	// NOT best-effort: unlike the receipt Acks below, a failed marker
	// write here is not a benign "Consume will just re-claim instead" --
	// this wake attempt is ABOUT to leave the lease open specifically so
	// Consume can reuse it later, and Consume can only do that by matching
	// this marker. An unmarked-but-still-open lease is indistinguishable
	// from an independent external claimant's lease (a second, deeper
	// review found exactly this: a swallowed failure here reproduces the
	// original stranded-delivery bug with no crash required, since Consume
	// correctly refuses to touch an unmarked active lease and a fresh
	// Claim attempt then fails with ErrAlreadyClaimed against this wake's
	// own still-live lease). So a failure here is treated the same as any
	// other post-claim failure this function already Nacks for retry
	// (offline/busy/stale-generation/turn-submit-failed): release the
	// lease now, while it's still cheap to do so, rather than leave it
	// open in a state neither Consume nor a future wake attempt can safely
	// reuse.
	if err := st.SetPendingReceiptLease(ctx, messageID, lease.AttemptID, lease.LeaseToken); err != nil {
		log.Printf("app: attempt wake: recording pending-receipt marker for message %s failed, releasing the claim for retry rather than leaving an unmarked lease open: %v", messageID, err)
		nackRetryable(ctx, ds, lease, "failed to record pending-receipt marker: "+err.Error(), wakeBusyRetryBackoff)
		return api.WakeOutcome{Attempted: true, SessionID: sessionID, Reason: "marker-write-failed", Detail: err.Error()}
	}

	// Real, timely host-accepted receipt: recorded now, at the moment
	// Tether is about to hand this delivery to a concrete session -- not
	// synthesized later at Consume time the way the consume-only path
	// (delivery_store.go's recordConsumedReceipts) bundles all three
	// stages together after the fact.
	if _, _, err := ds.Ack(ctx, delivery.AckRequest{Lease: lease, Stage: delivery.StageHostAccepted}); err != nil {
		log.Printf("app: attempt wake: ack host_accepted for delivery %s failed (best-effort receipt recording skipped): %v", deliveryID, err)
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
	// Recheck after the health call, immediately before submitting. A new
	// generation can reuse the same session ID or make it pull-only; neither
	// grants this in-flight claim authority to submit. A rebind between this
	// check and SendTurn remains a residual race, since they are not atomic.
	if _, blocked := checkWakeBinding(ctx, reg, to, sessionID, bindingGeneration); blocked.Reason != "" {
		nackRetryable(ctx, ds, lease, blocked.Reason+": "+blocked.Detail, wakeOfflineRetryBackoff)
		return blocked
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
	// Hold the lease, still at turn_submitted, for the recipient to consume
	// -- see wakeConsumeWindow. Best-effort: on failure the lease lapses at
	// wakeClaimLeaseDuration and the delivery is retried, as before.
	if _, err := ds.ExtendLease(ctx, lease, time.Now().Add(wakeConsumeWindow)); err != nil {
		log.Printf("app: attempt wake: extend lease for delivery %s to await consumption failed (it will lapse and retry): %v", deliveryID, err)
	}
	return api.WakeOutcome{Attempted: true, Delivered: true, SessionID: sessionID}
}

// checkWakeBinding fences actor wakes without consulting runtime health.
// expectedGeneration is zero before claim, and the claimed generation at
// submission. Never-bound and lapsed hosted actors keep the legacy path;
// the most recent pull-only binding keeps its fence even after it lapses.
func checkWakeBinding(ctx context.Context, reg *registry.Service, to messaging.Address, sessionID string, expectedGeneration int64) (int64, api.WakeOutcome) {
	if to.Kind != messaging.KindAgent || reg == nil {
		return 0, api.WakeOutcome{}
	}
	blocked := func(reason, detail string) (int64, api.WakeOutcome) {
		return 0, api.WakeOutcome{Reason: reason, Detail: detail, SessionID: sessionID}
	}
	target := registry.LogicalAgentBindingTarget(to.ID)
	current, err := reg.CurrentBinding(ctx, target)
	if err == nil {
		if isPullOnly(current) {
			return blocked("pull-only", "actor binding requires recipient polling")
		}
		if current.SessionID != sessionID || (expectedGeneration != 0 && current.Generation != expectedGeneration) {
			return blocked("stale-generation", "actor binding changed before turn submission")
		}
		return current.Generation, api.WakeOutcome{}
	}
	if !errors.Is(err, registry.ErrBindingNotFound) {
		return blocked("binding-check-failed", err.Error())
	}
	if expectedGeneration != 0 {
		return blocked("stale-generation", "claimed actor binding is no longer live")
	}
	history, err := reg.ListBindingsForTarget(ctx, target)
	if err != nil {
		return blocked("binding-check-failed", err.Error())
	}
	if len(history) > 0 && isPullOnly(history[0]) {
		return blocked("pull-only", "actor's most recent binding requires recipient polling")
	}
	return 0, api.WakeOutcome{}
}

func sendTurnDirect(ctx context.Context, reg *registry.Service, rt wakeRuntime, to messaging.Address, sessionID, wakeText string, bindingGeneration int64) api.WakeOutcome {
	if _, blocked := checkWakeBinding(ctx, reg, to, sessionID, bindingGeneration); blocked.Reason != "" {
		return blocked
	}
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
		sessionID, err := resolveActorSession(ctx, st, reg, rt, to.ID)
		if errors.Is(err, api.ErrBoundSessionNotRunning) {
			// No live session, like any other offline recipient: the
			// sweep parks it rather than logging a resolve failure.
			return "", nil
		}
		return sessionID, err
	default:
		return "", nil
	}
}

// runWakeSweep runs one pass. park carries parked deliveries across passes;
// nil gives this pass a fresh, empty set.
func runWakeSweep(ctx context.Context, st *store.Store, reg *registry.Service, rt wakeRuntime, park *wakeParkSet) (int, error) {
	if st == nil {
		return 0, nil
	}
	if park == nil {
		park = &wakeParkSet{}
	}
	ds := st.DeliveryStore()
	// At most park.size() of the listed rows can be parked, so reading
	// that many past the batch guarantees a full batch of unparked rows
	// whenever that many are ready: parked rows at the head of the queue
	// can no longer hide the rows behind them.
	limit := wakeSweepBatchLimit + park.size()
	ready, err := ds.ListDeliveries(ctx, delivery.Filter{
		Status:    []delivery.DeliveryStatus{delivery.DeliveryPending, delivery.DeliveryRetryScheduled},
		ReadyOnly: true,
		Limit:     limit,
	})
	if err != nil {
		return 0, fmt.Errorf("run wake sweep: list deliveries: %w", err)
	}

	now := time.Now()
	attempted, processed, newlyParked, unparkable := 0, 0, 0, 0
	more := false
	parkRow := func(id delivery.DeliveryID) {
		misses, ok := park.park(id, now)
		switch {
		case !ok:
			unparkable++
		case misses == 1:
			newlyParked++
		}
	}
	for _, rd := range ready {
		if ctx.Err() != nil {
			break
		}
		if park.parked(rd.ID, now) {
			continue
		}
		if processed == wakeSweepBatchLimit {
			more = true
			break
		}
		processed++
		sessionID, resolveErr := resolveWakeTarget(ctx, st, reg, rt, rd.Recipient)
		if resolveErr != nil {
			log.Printf("app: wake sweep: resolve session for %s failed: %v", rd.Recipient.URN(), resolveErr)
			parkRow(rd.ID)
			continue
		}
		if sessionID == "" {
			// No live session for this recipient. Park it rather than
			// re-resolve it every tick; it is re-checked once the park
			// lapses, and Consume or a bridge can still claim it meanwhile.
			parkRow(rd.ID)
			continue
		}
		park.release(rd.ID)
		msg, err := st.MessagingStore().Get(ctx, string(rd.MessageID))
		if err != nil {
			log.Printf("app: wake sweep: get message %s failed: %v", rd.MessageID, err)
			parkRow(rd.ID)
			continue
		}
		outcome := attemptWake(ctx, st, reg, rt, string(rd.MessageID), rd.Recipient, sessionID, sweepWakeText(msg))
		if outcome.Attempted {
			attempted++
		}
	}
	if len(ready) < limit {
		// The listing held every ready delivery: forget parked ones that
		// have since been delivered, consumed or claimed elsewhere.
		park.retain(ready)
	}
	if newlyParked > 0 {
		log.Printf("app: wake sweep: parked %d deliveries with no live recipient session (%d parked in total)", newlyParked, park.size())
	}
	if unparkable > 0 {
		log.Printf("app: wake sweep: park set full (%d); %d unresolvable deliveries will be re-checked every sweep", wakeParkMaxEntries, unparkable)
	}
	if more || (processed == wakeSweepBatchLimit && len(ready) == limit) {
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
