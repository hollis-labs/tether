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
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	messaging "github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/libs/mesh/core/messaging/delivery"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/messaging/wakeintent"
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
	// wakeSweepScanPage / wakeSweepScanBudget bound how many ready rows one
	// pass reads (a page per query, a budget of rows per pass). A pass
	// continues from where the previous one stopped, so a backlog of any
	// depth is walked across passes and every ready row is reached; they
	// only bound the work per pass.
	wakeSweepScanPage   = 512
	wakeSweepScanBudget = 4096
	// wakeSweepDeferredMax caps live rows carried to the next pass when the
	// batch was spent; beyond it they are rediscovered by the scan.
	wakeSweepDeferredMax = 4096
	// wakeParkEntryTTL drops a park entry whose row the scan has not seen
	// for this long (settled or claimed elsewhere), so the set cannot leak.
	wakeParkEntryTTL = 10 * time.Minute
)

// wakeMaxMessageAge bounds how old a message may be for the sweep to wake a
// recipient for it. Binding a URN to a live session makes the agent the
// recipient of mail addressed to that URN earlier; without a bound, that
// backlog replays as one turn per old message (13 three-day-old mails woke
// a portfolio manager on 2026-10-13). A message older than this stays in the
// inbox, unread and consumable, but is never woken for by the sweep; the
// recipient pulls it with tether_message_inbox. Zero or negative disables
// the bound. A variable so tests can shorten it.
var wakeMaxMessageAge = 6 * time.Hour

// Retry backoffs for a Nacked wake attempt: short for "try again once the
// current turn probably finished," longer for "nothing observable changed."
// Variables so tests can make a nacked delivery immediately ready again.
var (
	wakeBusyRetryBackoff    = 5 * time.Second
	wakeOfflineRetryBackoff = 30 * time.Second
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
//
// It is also the sweep's cross-pass scheduling state: the scan cursor, the
// live rows a spent batch left over, and the recipient the last batch
// stopped at, so the sweep walks a deep backlog and rotates fairly through
// recipients instead of restarting from the oldest row every pass.
type wakeParkSet struct {
	mu      sync.Mutex
	entries map[delivery.DeliveryID]wakeParkEntry

	cursor     delivery.DeliveryID // last row scanned; "" = start of queue
	highWater  delivery.DeliveryID // upper bound of the current scan cycle
	deferred   []delivery.RecipientDelivery
	lastServed string // URN of the recipient that took the last batch slot

	// Test overrides for the per-pass scan bounds; zero means the constants.
	scanPage, scanBudget int
}

// parkIneligible parks id like park, flagged so that recipient liveness does
// not release it.
func (p *wakeParkSet) parkIneligible(id delivery.DeliveryID, now time.Time) {
	if _, ok := p.park(id, now); !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entries[id]
	e.ineligible = true
	p.entries[id] = e
}

// holdsIneligible reports whether id is parked as ineligible and the park
// has not lapsed.
func (p *wakeParkSet) holdsIneligible(id delivery.DeliveryID, now time.Time) (held, flagged bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[id]
	if !ok || !e.ineligible {
		return false, false
	}
	return now.Before(e.until), true
}

// prune drops entries not seen within wakeParkEntryTTL.
func (p *wakeParkSet) prune(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, e := range p.entries {
		if now.Sub(e.seen) > wakeParkEntryTTL {
			delete(p.entries, id)
		}
	}
}

// touch records that the scan saw a parked row.
func (p *wakeParkSet) touch(id delivery.DeliveryID, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[id]; ok {
		e.seen = now
		p.entries[id] = e
	}
}

type wakeParkEntry struct {
	until  time.Time
	seen   time.Time
	misses int
	// ineligible marks a row parked because it is not the sweep's to wake
	// (no-wake intent, already answered), not because its recipient is
	// offline: a live recipient does not release it.
	ineligible bool
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
	e.seen = now
	p.entries[id] = e
	return e.misses, true
}

func (p *wakeParkSet) release(id delivery.DeliveryID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, id)
}

// retain drops every entry at or below upTo that is not in ready. Only call
// it with a complete listing of ready deliveries up to upTo: an entry missing
// from a truncated one may simply lie past the listing's end. Entries above
// upTo arrived after the cycle began and are left alone.
func (p *wakeParkSet) retain(ready []delivery.RecipientDelivery, upTo delivery.DeliveryID) {
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
		if id > upTo {
			continue
		}
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
	resolveOutpost func(context.Context, messaging.Address) (string, bool)
	sendMail       func(context.Context, string, string, string) error
	health         func(sessionID string) (api.RuntimeHealthResult, bool)
	sendTurn       func(ctx context.Context, sessionID, text string) error
}

func (s *Service) runtimeSeam() wakeRuntime {
	return wakeRuntime{
		health: func(id string) (api.RuntimeHealthResult, bool) {
			if strings.HasPrefix(id, "outpost:") {
				return s.outpostHealth(id)
			}
			return s.RuntimeHealth(id)
		},
		sendTurn:       s.SendTurn,
		resolveOutpost: s.outpostResolve,
		sendMail:       s.outpostSend,
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
	ctx = s.mailWakeInputContext(ctx, messageID)
	if env, err := s.Store.MessagingStore().Get(ctx, messageID); err == nil && env.Metadata[wakeintent.MetadataKey] != "" {
		intent, intentErr := wakeintent.Read(env)
		if intentErr != nil {
			return api.WakeOutcome{Reason: "invalid-wake-intent", Detail: intentErr.Error()}
		}
		if intent.NoWake {
			return api.WakeOutcome{Reason: "no-wake"}
		}
		return attemptRecipientWake(ctx, s.Store, s.Registry, s.runtimeSeam(), messageID, to, sessionID, wakeText)
	}
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

func attemptWake(ctx context.Context, st *store.Store, reg *registry.Service, rt wakeRuntime, messageID string, to messaging.Address, sessionID, wakeText string) (out api.WakeOutcome) {
	var observationAttempt string
	var observationNumber int
	var observationGeneration int64
	submitted := false
	recordSubmission := func() {
		submitted = true
		if st != nil {
			if err := st.RecordMessageWake(context.WithoutCancel(ctx), messageID, to.URN(), "wake.attempted", observationAttempt, observationNumber, observationGeneration, true, false, ""); err != nil {
				log.Printf("app: record wake submission: %v", err)
			}
		}
	}
	if st != nil {
		defer func() {
			if err := st.RecordMessageWake(context.WithoutCancel(ctx), messageID, to.URN(), "wake.outcome", observationAttempt, observationNumber, observationGeneration, submitted, out.Delivered, out.Reason); err != nil {
				log.Printf("app: record wake outcome: %v", err)
			}
		}()
	}

	if sessionID == "" {
		return api.WakeOutcome{Reason: "offline"}
	}
	// Explicit notify overrides and untracked legacy messages must respect
	// the same actor fence as the sweep's binding-first resolution.
	bindingGeneration, blocked := checkWakeBinding(ctx, reg, to, sessionID, 0)
	if blocked.Reason != "" {
		return blocked
	}
	observationGeneration = bindingGeneration
	if st == nil {
		return sendTurnDirect(ctx, reg, rt, to, sessionID, wakeText, bindingGeneration, recordSubmission)
	}

	deliveryID, ok, err := st.DeliveryIDForMessage(ctx, messageID)
	if err != nil {
		log.Printf("app: attempt wake: delivery id lookup for message %s failed (falling back to untracked wake): %v", messageID, err)
	}
	if err != nil || !ok {
		// Pre-T03 legacy message (no delivery-core tracking) or a lookup
		// failure: fall back to a direct, untracked wake rather than
		// blocking the caller's notify request on a bookkeeping gap.
		return sendTurnDirect(ctx, reg, rt, to, sessionID, wakeText, bindingGeneration, recordSubmission)
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
	observationAttempt = string(claim.Attempt.ID)
	observationNumber = claim.Delivery.AttemptCount
	observationGeneration = claim.Attempt.BindingGeneration

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

	recordSubmission()
	var submitErr error
	if rt.sendMail != nil {
		submitErr = rt.sendMail(ctx, sessionID, messageID, wakeText)
	} else {
		submitErr = rt.sendTurn(ctx, sessionID, wakeText)
	}
	if err := submitErr; err != nil {
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
	if strings.HasPrefix(sessionID, "outpost:") {
		// A real adapter acknowledgement marks delivered, never read/consumed.
		if _, err := st.DB().ExecContext(ctx, `UPDATE messages SET delivered_at=COALESCE(delivered_at,?) WHERE id=? AND to_urn=?`, time.Now().UTC().Format(time.RFC3339Nano), messageID, to.URN()); err != nil {
			return api.WakeOutcome{Attempted: true, Delivered: true, SessionID: sessionID, Detail: "delivered timestamp: " + err.Error()}
		}
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
	if strings.HasPrefix(sessionID, "outpost:") {
		target = to.URN()
	}
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

func sendTurnDirect(ctx context.Context, reg *registry.Service, rt wakeRuntime, to messaging.Address, sessionID, wakeText string, bindingGeneration int64, beforeSubmit func()) api.WakeOutcome {
	if _, blocked := checkWakeBinding(ctx, reg, to, sessionID, bindingGeneration); blocked.Reason != "" {
		return blocked
	}
	beforeSubmit()
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
	if rt.resolveOutpost != nil {
		if id, matched := rt.resolveOutpost(ctx, to); matched {
			return id, nil
		}
	}
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

// runWakeSweep runs one pass. park carries scheduling state across passes;
// nil gives this pass a fresh, empty one.
//
// The batch limit bounds wake attempts, not rows looked at: resolving a
// recipient is a cheap in-process lookup (memoized per recipient within the
// pass), while an attempt claims the delivery and submits a turn. Rows for
// recipients with no live session are parked without spending any of the
// batch. Each pass reads a bounded slice of the ready queue starting after
// where the previous pass stopped, so a backlog of any depth is walked
// across passes and no ready row is unreachable; the cursor wraps at the end
// of the queue. Live rows are served round-robin across recipients, starting
// after the recipient the previous batch ended on, oldest first within a
// recipient; rows a spent batch leaves over are carried to the next pass. A
// parked row whose recipient has a live session again is released and woken
// at once rather than when its park lapses.
func runWakeSweep(ctx context.Context, st *store.Store, reg *registry.Service, rt wakeRuntime, park *wakeParkSet) (int, error) {
	if st == nil {
		return 0, nil
	}
	if park == nil {
		park = &wakeParkSet{}
	}
	const batchLimit = wakeSweepBatchLimit
	scanPage, scanBudget := wakeSweepScanPage, wakeSweepScanBudget
	park.mu.Lock()
	if park.scanPage > 0 {
		scanPage = park.scanPage
	}
	if park.scanBudget > 0 {
		scanBudget = park.scanBudget
	}
	park.mu.Unlock()
	now := time.Now()

	type resolved struct {
		sessionID string
		err       error
	}
	memo := map[string]resolved{}
	resolve := func(to messaging.Address) resolved {
		key := to.URN()
		if r, ok := memo[key]; ok {
			return r
		}
		sessionID, err := resolveWakeTarget(ctx, st, reg, rt, to)
		r := resolved{sessionID, err}
		memo[key] = r
		return r
	}

	newlyParked, unparkable, scanned := 0, 0, 0
	type liveRow struct {
		rd        delivery.RecipientDelivery
		sessionID string
	}
	liveByRecipient := map[string][]liveRow{}
	seenLive := map[delivery.DeliveryID]bool{}
	addLive := func(rd delivery.RecipientDelivery, sessionID string) {
		if seenLive[rd.ID] {
			return
		}
		seenLive[rd.ID] = true
		key := rd.Recipient.URN()
		liveByRecipient[key] = append(liveByRecipient[key], liveRow{rd, sessionID})
	}

	// Rows a previous batch left over go first; a recipient that has since
	// gone offline just drops them (the scan re-finds and parks them).
	park.mu.Lock()
	carried := park.deferred
	park.deferred = nil
	cursor := park.cursor
	highWater := park.highWater
	park.mu.Unlock()
	for _, rd := range carried {
		if r := resolve(rd.Recipient); r.err == nil && r.sessionID != "" {
			addLive(rd, r.sessionID)
		}
	}

	startedAtHead := cursor == ""
	if startedAtHead {
		// A new scan cycle covers the rows that exist now, so it is finite
		// however fast new ones arrive, and a row that became ready again
		// behind the cursor (a busy Nack, a lapsed lease) is revisited next
		// cycle.
		hw, err := st.DeliveryHighWater(ctx)
		if err != nil {
			return 0, fmt.Errorf("run wake sweep: %w", err)
		}
		highWater = hw
	}
	wrapped := false
	var scannedRows []delivery.RecipientDelivery
	for scanned < scanBudget && ctx.Err() == nil {
		n := scanPage
		if rest := scanBudget - scanned; rest < n {
			n = rest
		}
		page, err := st.ReadyDeliveryPage(ctx, cursor, highWater, n)
		if err != nil {
			return 0, fmt.Errorf("run wake sweep: list deliveries: %w", err)
		}
		if page.LastID != "" {
			cursor = page.LastID
		}
		for _, rd := range page.Rows {
			scanned++
			scannedRows = append(scannedRows, rd)
			r := resolve(rd.Recipient)
			if r.sessionID == "" || r.err != nil {
				// No live session for this recipient. Park it rather than
				// re-wake it; Consume or a bridge can still claim it meanwhile.
				if park.parked(rd.ID, now) {
					park.touch(rd.ID, now)
					continue
				}
				if r.err != nil {
					log.Printf("app: wake sweep: resolve session for %s failed: %v", rd.Recipient.URN(), r.err)
				}
				misses, ok := park.park(rd.ID, now)
				switch {
				case !ok:
					unparkable++
				case misses == 1:
					newlyParked++
				}
				continue
			}
			if held, flagged := park.holdsIneligible(rd.ID, now); flagged {
				// Parked as not ours to wake; a live recipient does not
				// change that. Re-checked once the park lapses.
				if held {
					park.touch(rd.ID, now)
					continue
				}
			} else {
				park.release(rd.ID)
			}
			addLive(rd, r.sessionID)
		}
		if page.Exhausted {
			// The queue ended at the cycle's bound, not the budget running out.
			wrapped = true
			break
		}
		// More live work than the carry-over can hold: stop reading; the
		// cursor resumes here next pass.
		if len(seenLive) >= wakeSweepDeferredMax {
			break
		}
	}
	if wrapped {
		cursor = ""
	}

	// Round-robin across live recipients in URN order, starting after the
	// recipient the previous batch ended on; oldest delivery first within each.
	keys := make([]string, 0, len(liveByRecipient))
	for k, rows := range liveByRecipient {
		sort.Slice(rows, func(i, j int) bool { return rows[i].rd.ID < rows[j].rd.ID })
		keys = append(keys, k)
	}
	sort.Strings(keys)
	park.mu.Lock()
	last := park.lastServed
	park.mu.Unlock()
	start := sort.SearchStrings(keys, last)
	if start < len(keys) && keys[start] == last {
		start++
	}
	if start >= len(keys) {
		start = 0
	}
	ring := append(append([]string{}, keys[start:]...), keys[:start]...)

	attempted, processed := 0, 0
	var left []delivery.RecipientDelivery
	lastServed := last
	for round := 0; ; round++ {
		sawRow := false
		for _, key := range ring {
			rows := liveByRecipient[key]
			if round >= len(rows) {
				continue
			}
			sawRow = true
			lr := rows[round]
			if ctx.Err() != nil || processed >= batchLimit {
				if len(left) < wakeSweepDeferredMax {
					left = append(left, lr.rd)
				}
				continue
			}
			msg, err := st.MessagingStore().Get(ctx, string(lr.rd.MessageID))
			if err != nil {
				park.parkIneligible(lr.rd.ID, now)
				continue
			}
			if wakeMaxMessageAge > 0 && !msg.CreatedAt.IsZero() && now.Sub(msg.CreatedAt) > wakeMaxMessageAge {
				// Quiet backlog: old mail is left for the inbox, not replayed as a turn.
				park.parkIneligible(lr.rd.ID, now)
				continue
			}
			intent, intentErr := wakeintent.Read(msg)
			if intentErr != nil || intent.NoWake {
				park.parkIneligible(lr.rd.ID, now)
				continue
			}
			prior, found, priorErr := st.RecipientWakeOutcome(ctx, msg.ID)
			if priorErr != nil {
				park.parkIneligible(lr.rd.ID, now)
				continue
			}
			if found {
				var prev api.WakeOutcome
				if prior == "" || json.Unmarshal([]byte(prior), &prev) != nil || prev.Delivered {
					park.parkIneligible(lr.rd.ID, now)
					continue
				}
			}
			park.release(lr.rd.ID)
			processed++
			lastServed = key
			text := intent.WakeText
			if text == "" {
				text = sweepWakeText(msg)
			}
			var outcome api.WakeOutcome
			// A retried wake is the same mail wake as the first attempt: tag its input with the
			// message and the sender the hub stamped, as Service.AttemptWake does.
			wakeCtx := withTurnInput(ctx, mailWakeInputNote(msg.ID, msg.Metadata))
			if !found && st.HasMessageDeliveryObserver() {
				outcome = attemptRecipientWake(wakeCtx, st, reg, rt, string(lr.rd.MessageID), lr.rd.Recipient, lr.sessionID, text)
			} else {
				outcome = attemptWake(wakeCtx, st, reg, rt, string(lr.rd.MessageID), lr.rd.Recipient, lr.sessionID, text)
			}
			if found {
				raw, _ := json.Marshal(outcome)
				_ = st.CompleteRecipientWake(ctx, msg.ID, string(raw))
			}
			if outcome.Attempted {
				attempted++
			}
		}
		if !sawRow {
			break
		}
	}

	park.mu.Lock()
	park.cursor = cursor
	park.highWater = highWater
	park.deferred = left
	park.lastServed = lastServed
	park.mu.Unlock()
	if startedAtHead && wrapped {
		// The pass saw the whole ready queue: forget parked rows that have
		// since been delivered, consumed or claimed elsewhere.
		park.retain(scannedRows, highWater)
	}
	park.prune(now)
	deferred := len(left)
	// One summary line, and only when the pass attempted, parked or deferred something.
	if attempted > 0 || newlyParked > 0 || deferred > 0 {
		log.Printf("app: wake sweep: attempted %d, newly parked %d (%d parked in total), %d live deliveries deferred to the next sweep (batch limit %d), %d unparkable (park set cap %d), scanned %d",
			attempted, newlyParked, park.size(), deferred, batchLimit, unparkable, wakeParkMaxEntries, scanned)
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
	if intent, err := wakeintent.Read(env); err == nil && intent.Urgency != "" {
		urgency = intent.Urgency
	}
	if urgency == "" {
		urgency = "normal"
	}
	return fmt.Sprintf("**Mailbox wake (daemon-injected, retried)** — you have unread message(s) waiting. Latest message: `%s` from `%s` to `%s`, kind `%s`, urgency `%s`.\n\nPlease run your inbox-check procedure, process messages normally, and mark handled messages read or consumed according to your checklist. This is a delivery notification, not an instruction to abandon the current task unless your own procedure says the message is urgent. This reminder is the only push you get for these messages: marking a message read or consumed settles it, and a settled message is never woken again.",
		env.ID, env.From.URN(), env.To.URN(), env.Kind, urgency)
}

// mailWakeInputNote is the provenance of the mail being injected: the sender the hub stamped on
// the stored message. No verified stamp is an unknown sender.
func mailWakeInputNote(messageID string, meta map[string]string) turnInputNote {
	note := turnInputNote{Source: store.InputMailWake, Class: store.ClassUnknown, Basis: store.BasisNone, RefID: messageID}
	if meta != nil {
		note.SenderURN, note.Class, note.Basis = classifyStampedSender(meta)
	}
	return note
}

// mailWakeInputContext tags ctx with the provenance of the mail being injected. A message that
// cannot be read is an unknown sender. The sweep's retried wakes tag the same way
// (CW-20261021-0029): they are the same mail wake, only later.
func (s *Service) mailWakeInputContext(ctx context.Context, messageID string) context.Context {
	var meta map[string]string
	if s.Store != nil && messageID != "" {
		if env, err := s.Store.MessagingStore().Get(ctx, messageID); err == nil {
			meta = env.Metadata
		}
	}
	return withTurnInput(ctx, mailWakeInputNote(messageID, meta))
}
