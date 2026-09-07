package app

// wake_test.go — T06 (messaging vNext, CW-20260906-0037) acceptance
// evidence. Exercises resolveActorSession/attemptWake/runWakeSweep against
// a REAL *store.Store (real SQLite delivery core, real receipt stages) and
// a REAL *registry.Service (real generation-fenced RuntimeBinding leases),
// with only the OS-process runtime layer faked via wakeRuntime -- that
// layer belongs to agentkit and is already compliance-tested elsewhere
// (service_claude_code_test.go); Tether's own job here is proving its
// CONSUMPTION of RuntimeHealth/SendTurn is correct, not re-deriving
// agentkit's own process-management correctness.
//
// Acceptance #1 (idle/busy/offline, fresh durable activation, explicit-
// session expiry, concurrent sessions, stale-generation races):
//   TestResolveActorSession_BindingFirst_AuthoritativeOverLegacy
//   TestResolveActorSession_BoundOwnerOffline_NoFallbackReroute
//   TestResolveActorSession_NeverBound_LegacyFallback
//   TestResolveActorSession_ConcurrentSessions_NewerBindingWins
//   TestAttemptWake_IdleSession_DeliversAndRecordsReceipts
//   TestAttemptWake_BusySession_NacksWithoutSendTurn
//   TestAttemptWake_OfflineSession_NoClaimAttempted
//   TestResolveActorSession_ExplicitSessionExpiry_FallsBackToLegacy
//   TestResolveActorSession_ExpiredPullOnlyLease_StillFencesNoLegacyFallback
//   TestResolveActorSession_ReclaimedFromBridgeThenLapsed_FallsBackToLegacy
//
// Acceptance #2 (crash at persist/claim/host-accept/turn-submit/receipt
// boundaries leaves replayable evidence, no falsely acknowledged loss):
//   TestAttemptWake_ReceiptTrail_MatchesArchitectureVocabulary
//   TestAttemptWake_BusySession_NacksWithoutSendTurn (the real,
//     attemptWake-driven proof for the reachable claim/host-accepted/
//     turn-submitted boundary -- see its receipt assertions)
//   TestDeliveryCoreInvariant_NoReceiptOrConsumptionAheadOfItsCausingCall_AfterClaim
//   TestDeliveryCoreInvariant_NoReceiptOrConsumptionAheadOfItsCausingCall_AfterHostAccepted
//   TestDeliveryCoreInvariant_NoReceiptOrConsumptionAheadOfItsCausingCall_AfterTurnSubmitted
//     (these three drive the delivery core through attemptWake's own call
//     sequence directly, since attemptWake's current seam gives no way to
//     interrupt IT mid-function at these specific points -- see the
//     "Crash-boundary evidence" section comment below for why that's an
//     honest, deliberate test design rather than a gap)
//
// Acceptance #3 (receipts distinguish persisted/host_accepted/
// turn_submitted; unsupported steering fails honestly; a failed selected
// host never silently spawns a second local owner):
//   TestAttemptWake_TurnSubmitFails_NacksAndNeverTriesDifferentSession
//   TestAttemptWake_StaleGenerationRace_AbortsBeforeSendTurn
//   TestAttemptWake_ClaimUnavailable_ConcurrentAttemptNoDuplicate
//   TestRunWakeSweep_RetriesBusyDelivery_ThenDelivers

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"
	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/delivery"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

// fakeRuntime is a fully deterministic, in-memory wakeRuntime double.
// health/state are keyed by session ID; sendTurn is call-counted and can
// be scripted to fail.
type fakeRuntime struct {
	mu        sync.Mutex
	alive     map[string]bool
	state     map[string]agentsessions.LiveState
	sendErr   map[string]error
	sendCalls []sendTurnCall
}

type sendTurnCall struct {
	SessionID string
	Text      string
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{
		alive:   map[string]bool{},
		state:   map[string]agentsessions.LiveState{},
		sendErr: map[string]error{},
	}
}

func (f *fakeRuntime) setAlive(sessionID string, live bool, state agentsessions.LiveState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alive[sessionID] = live
	f.state[sessionID] = state
}

func (f *fakeRuntime) setSendErr(sessionID string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendErr[sessionID] = err
}

func (f *fakeRuntime) sendCallCount(sessionID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.sendCalls {
		if c.SessionID == sessionID {
			n++
		}
	}
	return n
}

func (f *fakeRuntime) seam() wakeRuntime {
	return wakeRuntime{
		health: func(sessionID string) (api.RuntimeHealthResult, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if !f.alive[sessionID] {
				return api.RuntimeHealthResult{}, false
			}
			return api.RuntimeHealthResult{
				SessionID: sessionID,
				Health:    agentsessions.HealthStatus{Alive: true, State: f.state[sessionID]},
			}, true
		},
		sendTurn: func(_ context.Context, sessionID, text string) error {
			f.mu.Lock()
			f.sendCalls = append(f.sendCalls, sendTurnCall{SessionID: sessionID, Text: text})
			err := f.sendErr[sessionID]
			f.mu.Unlock()
			return err
		},
	}
}

// newWakeHarness opens a real SQLite-backed *store.Store and a real
// *registry.Service sharing its DB -- the same composition
// app.Service.New itself uses (service.go), minus everything unrelated to
// wake/binding logic (no Manager, no runtime factories).
func newWakeHarness(t *testing.T) (*store.Store, *registry.Service) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "wake.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	reg := registry.NewService(registry.NewStorage(st.DB()))
	return st, reg
}

func sendMessage(t *testing.T, st *store.Store, to messaging.Address) messaging.Envelope {
	t.Helper()
	env, err := st.MessagingStore().Send(context.Background(), messaging.Envelope{
		Kind: messaging.MsgKindNotice,
		From: messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"},
		To:   to,
	})
	if err != nil {
		t.Fatalf("send message: %v", err)
	}
	return env
}

// ─── ResolveActorSession: binding-first authoritative resolution ──────────

func TestResolveActorSession_BindingFirst_AuthoritativeOverLegacy(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()

	if err := st.CreateSession(store.SessionRow{ID: "s-legacy", LogicalAgentID: "worker", State: "running"}, nil); err != nil {
		t.Fatalf("create legacy session: %v", err)
	}
	rt.setAlive("s-legacy", true, agentsessions.LiveStateIdle)
	rt.setAlive("s-bound", true, agentsessions.LiveStateIdle)

	if _, err := reg.LeaseBinding(ctx, registry.LogicalAgentBindingTarget("worker"), "s-bound", "local", "s-bound", nil, "", 0); err != nil {
		t.Fatalf("lease binding: %v", err)
	}

	got, err := resolveActorSession(ctx, st, reg, rt.seam(), "worker")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "s-bound" {
		t.Fatalf("resolveActorSession = %q, want %q (binding must win over the legacy scan even though s-legacy also matches and is alive)", got, "s-bound")
	}
}

func TestResolveActorSession_BoundOwnerOffline_NoFallbackReroute(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()

	if err := st.CreateSession(store.SessionRow{ID: "s-legacy", LogicalAgentID: "worker", State: "running"}, nil); err != nil {
		t.Fatalf("create legacy session: %v", err)
	}
	rt.setAlive("s-legacy", true, agentsessions.LiveStateIdle)
	// s-bound is leased but NOT alive -- the crashed/offline home host.
	rt.setAlive("s-bound", false, agentsessions.LiveStateIdle)

	if _, err := reg.LeaseBinding(ctx, registry.LogicalAgentBindingTarget("worker"), "s-bound", "local", "s-bound", nil, "", 0); err != nil {
		t.Fatalf("lease binding: %v", err)
	}

	got, err := resolveActorSession(ctx, st, reg, rt.seam(), "worker")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "" {
		t.Fatalf("resolveActorSession = %q, want \"\" (a bound-but-offline owner must never silently reroute to a different running session for the same actor)", got)
	}
}

func TestResolveActorSession_NeverBound_LegacyFallback(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()

	if err := st.CreateSession(store.SessionRow{ID: "s-legacy", LogicalAgentID: "worker", State: "running"}, nil); err != nil {
		t.Fatalf("create legacy session: %v", err)
	}
	rt.setAlive("s-legacy", true, agentsessions.LiveStateIdle)

	got, err := resolveActorSession(ctx, st, reg, rt.seam(), "worker")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "s-legacy" {
		t.Fatalf("resolveActorSession = %q, want %q (no binding ever leased: legacy compatibility fallback must still work)", got, "s-legacy")
	}
}

// TestResolveActorSession_ConcurrentSessions_NewerBindingWins is the
// concurrent-actor-sessions / stale-generation acceptance case: a second
// launch for the same logical agent leases a new binding generation, and
// resolution must follow the newer one, not the one that lost the race.
func TestResolveActorSession_ConcurrentSessions_NewerBindingWins(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	rt.setAlive("s2", true, agentsessions.LiveStateIdle)

	target := registry.LogicalAgentBindingTarget("worker")
	if _, err := reg.LeaseBinding(ctx, target, "s1", "local", "s1", nil, "", 0); err != nil {
		t.Fatalf("lease s1: %v", err)
	}
	if _, err := reg.LeaseBinding(ctx, target, "s2", "local", "s2", nil, "", 0); err != nil {
		t.Fatalf("lease s2: %v", err)
	}

	got, err := resolveActorSession(ctx, st, reg, rt.seam(), "worker")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "s2" {
		t.Fatalf("resolveActorSession = %q, want %q (the newer generation must win)", got, "s2")
	}
}

// TestResolveActorSession_ExplicitSessionExpiry_FallsBackToLegacy is the
// explicit-session-expiry acceptance case: CurrentBinding excludes a
// lease once its TTL lapses (bindings.go: "lease_expires_at IS NULL OR
// lease_expires_at > now"), which is a DIFFERENT path than "never bound"
// (registry.ErrBindingNotFound either way, but reached via expiry here,
// not absence) -- resolution must still work by honestly falling back to
// the legacy scan rather than getting stuck reporting offline forever
// just because the old lease's clock ran out.
func TestResolveActorSession_ExplicitSessionExpiry_FallsBackToLegacy(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s-expired-owner", true, agentsessions.LiveStateIdle)
	rt.setAlive("s-legacy", true, agentsessions.LiveStateIdle)

	if err := st.CreateSession(store.SessionRow{ID: "s-legacy", LogicalAgentID: "worker", State: "running"}, nil); err != nil {
		t.Fatalf("create legacy session: %v", err)
	}

	target := registry.LogicalAgentBindingTarget("worker")
	if _, err := reg.LeaseBinding(ctx, target, "s-expired-owner", "local", "s-expired-owner", nil, "", 20*time.Millisecond); err != nil {
		t.Fatalf("lease with short ttl: %v", err)
	}

	// While the lease is still live, it must be authoritative.
	got, err := resolveActorSession(ctx, st, reg, rt.seam(), "worker")
	if err != nil {
		t.Fatalf("resolve (pre-expiry): %v", err)
	}
	if got != "s-expired-owner" {
		t.Fatalf("resolveActorSession (pre-expiry) = %q, want %q", got, "s-expired-owner")
	}

	time.Sleep(40 * time.Millisecond)

	got, err = resolveActorSession(ctx, st, reg, rt.seam(), "worker")
	if err != nil {
		t.Fatalf("resolve (post-expiry): %v", err)
	}
	if got != "s-legacy" {
		t.Fatalf("resolveActorSession (post-expiry) = %q, want %q (an expired lease must fall back to the legacy scan, not report a permanently stuck offline actor)", got, "s-legacy")
	}
}

// TestResolveActorSession_ExpiredPullOnlyLease_StillFencesNoLegacyFallback
// is the negative case a distinct review pass on T06/T07 found missing: a
// PULL-ONLY binding's lease expiring must NOT behave like
// TestResolveActorSession_ExplicitSessionExpiry_FallsBackToLegacy above --
// CurrentBinding's ErrBindingNotFound is ambiguous between "never leased"
// and "leased but lapsed," and only the pull-only case must keep fencing
// after its own lease lapses (e.g. a published-local bridge that missed a
// renewal window). Falling through to the legacy scan here would let an
// unrelated same-logical-agent-ID running session receive a wake that was
// never authorized -- exactly the unauthorized reroute the architecture
// and this file's own doc comment forbid.
func TestResolveActorSession_ExpiredPullOnlyLease_StillFencesNoLegacyFallback(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("bridge-session-1", true, agentsessions.LiveStateIdle)
	rt.setAlive("s-unrelated-legacy", true, agentsessions.LiveStateIdle)

	if err := st.CreateSession(store.SessionRow{ID: "s-unrelated-legacy", LogicalAgentID: "worker", State: "running"}, nil); err != nil {
		t.Fatalf("create legacy session: %v", err)
	}

	target := registry.LogicalAgentBindingTarget("worker")
	if _, err := reg.LeaseBinding(ctx, target, "bridge-session-1", "external-bridge-host", "attempt-1",
		[]string{"pull-only"}, registry.VisibilityPublishedLocal, 20*time.Millisecond); err != nil {
		t.Fatalf("lease pull-only binding with short ttl: %v", err)
	}

	// While the lease is still live, pull-only fencing applies as already
	// covered by TestResolveActorSession_PullOnlyBinding_NeverResolvesEvenWhenHealthWouldMatch.
	time.Sleep(40 * time.Millisecond)

	got, err := resolveActorSession(ctx, st, reg, rt.seam(), "worker")
	if err != nil {
		t.Fatalf("resolve (post-expiry): %v", err)
	}
	if got != "" {
		t.Fatalf("resolveActorSession (pull-only lease expired) = %q, want \"\" -- an unrenewed published-local bridge must never be silently rerouted to an unrelated running session for the same logical agent", got)
	}
}

// TestResolveActorSession_ReclaimedFromBridgeThenLapsed_FallsBackToLegacy
// is the negative case a SECOND distinct review pass found the first fix
// above got wrong: it originally checked pull-only across the target's
// ENTIRE binding history, not just its most recent generation. That is too
// broad -- a target that was briefly bridge-bound, then legitimately
// reclaimed by Tether's own internal launch path (a fresh, non-pull-only
// generation superseding the bridge's), and whose Tether-hosted session
// later stopped normally (revoking ITS OWN binding), must fall through to
// the legacy heuristic like any other ordinary stopped session -- not stay
// permanently fenced by a bridge generation that is no longer the actor's
// most recent owner of record. Only the highest generation's visibility
// should ever decide this, matching CurrentBinding's own generation-
// fencing rule for the live case.
func TestResolveActorSession_ReclaimedFromBridgeThenLapsed_FallsBackToLegacy(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s-unrelated-legacy", true, agentsessions.LiveStateIdle)

	if err := st.CreateSession(store.SessionRow{ID: "s-unrelated-legacy", LogicalAgentID: "worker", State: "running"}, nil); err != nil {
		t.Fatalf("create legacy session: %v", err)
	}

	target := registry.LogicalAgentBindingTarget("worker")
	// Gen 1: a bridge leases pull-only with a short ttl that lapses.
	if _, err := reg.LeaseBinding(ctx, target, "bridge-session-1", "external-bridge-host", "attempt-1",
		[]string{"pull-only"}, registry.VisibilityPublishedLocal, 20*time.Millisecond); err != nil {
		t.Fatalf("lease pull-only gen 1: %v", err)
	}
	time.Sleep(40 * time.Millisecond)

	// Gen 2: Tether's own launch path reclaims the target (private-local,
	// no ttl) -- the legitimate exemption api/bindings.go documents.
	reclaimed, err := reg.LeaseBinding(ctx, target, "s-reclaimed", "local", "s-reclaimed", nil, registry.VisibilityPrivateLocal, 0)
	if err != nil {
		t.Fatalf("lease private-local gen 2 (reclaim): %v", err)
	}

	// Gen 2's own session later stops normally and revokes its binding --
	// an ordinary lifecycle event, unrelated to the earlier bridge.
	if err := reg.RevokeBinding(ctx, reclaimed.ID); err != nil {
		t.Fatalf("revoke gen 2: %v", err)
	}

	got, err := resolveActorSession(ctx, st, reg, rt.seam(), "worker")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "s-unrelated-legacy" {
		t.Fatalf("resolveActorSession = %q, want %q -- the actor's most recent binding (gen 2) was never pull-only, so its later revocation must fall through to the legacy heuristic like any other stopped session, not stay fenced by an older, superseded bridge generation", got, "s-unrelated-legacy")
	}
}

// ─── T07: published-local (pull-only) bridge bindings never wake-target ───

// TestResolveActorSession_PullOnlyBinding_NeverResolvesEvenWhenHealthWouldMatch
// proves the pull-only check happens BEFORE the health lookup: even if a
// pull-only binding's self-asserted SessionID happens to collide with a
// real, alive local session (a bridge could pick any string), resolution
// must still refuse to target it -- Tether never assumes launch/resume
// authority over a published-local actor, full stop, not "only when its
// self-asserted id happens not to resolve."
func TestResolveActorSession_PullOnlyBinding_NeverResolvesEvenWhenHealthWouldMatch(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("bridge-session-1", true, agentsessions.LiveStateIdle)

	if _, err := reg.LeaseBinding(ctx, registry.LogicalAgentBindingTarget("worker"), "bridge-session-1", "external-bridge-host", "attempt-1", []string{"pull-only"}, registry.VisibilityPublishedLocal, 0); err != nil {
		t.Fatalf("lease pull-only binding: %v", err)
	}

	got, err := resolveActorSession(ctx, st, reg, rt.seam(), "worker")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "" {
		t.Fatalf("resolveActorSession = %q, want \"\" -- a pull-only published-local binding must never be wake-targeted, even though its self-asserted session id happens to be alive", got)
	}
}

// TestAttemptWake_PullOnlyTarget_NotifyStoresButNeverAttemptsWake is the
// end-to-end "unauthorized wake" proof at the notify boundary: a message
// addressed to a pull-only-bound actor is still durably stored (the
// mailbox always accepts it), but resolveActorSession's "" means
// AttemptWake is never even called, so no Claim, no host_accepted, no
// SendTurn -- the bridge is expected to pull it via GET /messages/inbox on
// its own schedule instead.
func TestAttemptWake_PullOnlyTarget_NotifyNeverResolvesASessionToWake(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()

	if _, err := reg.LeaseBinding(ctx, registry.LogicalAgentBindingTarget("worker"), "bridge-session-1", "external-bridge-host", "attempt-1", []string{"pull-only"}, registry.VisibilityPublishedLocal, 0); err != nil {
		t.Fatalf("lease pull-only binding: %v", err)
	}

	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	env := sendMessage(t, st, to)

	sessionID, err := resolveActorSession(ctx, st, reg, rt.seam(), "worker")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if sessionID != "" {
		t.Fatalf("resolved sessionID = %q, want \"\" -- notify must never attempt a wake for a pull-only actor", sessionID)
	}

	// The message itself is still safely in the durable mailbox for the
	// bridge to pull later -- pull-only never means "message dropped."
	deliveryID, ok, err := st.DeliveryIDForMessage(ctx, env.ID)
	if err != nil || !ok {
		t.Fatalf("delivery id lookup: ok=%v err=%v", ok, err)
	}
	rd, err := st.DeliveryStore().GetDelivery(ctx, delivery.DeliveryID(deliveryID))
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if rd.Status != delivery.DeliveryPending {
		t.Fatalf("delivery status = %q, want pending (untouched -- no Claim was ever attempted for a pull-only target)", rd.Status)
	}
	if rd.AttemptCount != 0 {
		t.Fatalf("attempt_count = %d, want 0", rd.AttemptCount)
	}
}

// TestRunWakeSweep_PullOnlyTarget_NeverAttemptedAndNeverBurnsCycles proves
// the sweep pump also respects the pull-only declaration -- it must not
// loop forever trying (and failing) to wake a target that will never be
// wake-targetable, wasting sweep cycles on a permanently no-op case.
func TestRunWakeSweep_PullOnlyTarget_NeverAttemptedAndNeverBurnsCycles(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()

	if _, err := reg.LeaseBinding(ctx, registry.LogicalAgentBindingTarget("worker"), "bridge-session-1", "external-bridge-host", "attempt-1", []string{"pull-only"}, registry.VisibilityPublishedLocal, 0); err != nil {
		t.Fatalf("lease pull-only binding: %v", err)
	}
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	sendMessage(t, st, to)

	n, err := runWakeSweep(ctx, st, reg, rt.seam())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("sweep attempted %d deliveries for a pull-only target, want 0", n)
	}
}

// ─── AttemptWake: real Claim/Ack/Nack sequencing ───────────────────────────

func TestAttemptWake_IdleSession_DeliversAndRecordsReceipts(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s1", true, agentsessions.LiveStateIdle)

	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	env := sendMessage(t, st, to)

	outcome := attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "s1", "wake up")
	if !outcome.Delivered || outcome.Reason != "" {
		t.Fatalf("outcome = %+v, want Delivered=true Reason=\"\"", outcome)
	}
	if rt.sendCallCount("s1") != 1 {
		t.Fatalf("sendTurn called %d times, want exactly 1", rt.sendCallCount("s1"))
	}

	deliveryID, ok, err := st.DeliveryIDForMessage(ctx, env.ID)
	if err != nil || !ok {
		t.Fatalf("delivery id lookup: ok=%v err=%v", ok, err)
	}
	receipts, err := st.DeliveryStore().Receipts(ctx, delivery.DeliveryID(deliveryID))
	if err != nil {
		t.Fatalf("receipts: %v", err)
	}
	var stages []delivery.ReceiptStage
	for _, r := range receipts {
		stages = append(stages, r.Stage)
	}
	wantSuffix := []delivery.ReceiptStage{delivery.StageHostAccepted, delivery.StageTurnSubmitted}
	if len(stages) < 2 || stages[len(stages)-2] != wantSuffix[0] || stages[len(stages)-1] != wantSuffix[1] {
		t.Fatalf("receipt stages = %v, want to end with %v (real, timely host_accepted then turn_submitted, not synthesized at consume time)", stages, wantSuffix)
	}
}

// TestAttemptWake_PendingReceiptMarkerWriteFails_NacksInsteadOfProceeding
// is the regression test for a gap a second independent review found in
// the hijack-safety fix above: attemptWake originally treated
// SetPendingReceiptLease as best-effort, logging and continuing straight
// to SendTurn on failure. But that marker is what lets Consume later prove
// this exact lease is safe to reuse -- an unmarked-but-still-open lease is
// indistinguishable from an independent external claimant's (T07 bridge)
// lease, so Consume correctly refuses to touch it, a fresh Claim then
// fails with ErrAlreadyClaimed against this wake's own still-live lease,
// and the delivery is stranded exactly as before the whole fix -- no crash
// required, reproduced by a marker-write failure alone. Fixed: a marker
// write failure now Nacks the just-acquired lease for retry (the same
// pattern already used for offline/busy/stale-generation/turn-submit
// failures) instead of proceeding.
//
// A real UPDATE failure is simulated with a genuine SQLite trigger that
// rejects writes to the marker column specifically -- Claim/Ack (against
// go-messaging's own separate tables) are completely unaffected, so this
// exercises the real failure path, not a mocked one.
func TestAttemptWake_PendingReceiptMarkerWriteFails_NacksInsteadOfProceeding(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s1", true, agentsessions.LiveStateIdle)

	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	env := sendMessage(t, st, to)

	if _, err := st.DB().ExecContext(ctx, `
		CREATE TRIGGER reject_pending_receipt_marker
		BEFORE UPDATE OF pending_receipt_attempt_id ON messages
		BEGIN SELECT RAISE(ABORT, 'simulated marker write failure'); END;
	`); err != nil {
		t.Fatalf("install trigger: %v", err)
	}

	outcome := attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "s1", "wake up")
	if outcome.Delivered {
		t.Fatalf("outcome = %+v, want Delivered=false", outcome)
	}
	if outcome.Reason != "marker-write-failed" {
		t.Fatalf("outcome.Reason = %q, want marker-write-failed", outcome.Reason)
	}
	if rt.sendCallCount("s1") != 0 {
		t.Fatalf("sendTurn called %d times, want 0 -- must abort before submitting a turn against an unmarked lease", rt.sendCallCount("s1"))
	}

	deliveryID, ok, err := st.DeliveryIDForMessage(ctx, env.ID)
	if err != nil || !ok {
		t.Fatalf("delivery id lookup: ok=%v err=%v", ok, err)
	}
	del, err := st.DeliveryStore().GetDelivery(ctx, delivery.DeliveryID(deliveryID))
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if del.Status == delivery.DeliveryLeased {
		t.Fatalf("delivery status = leased after a failed marker write, want the lease released (retry_scheduled) -- an unmarked lease must not be left open")
	}
}

func TestAttemptWake_BusySession_NacksWithoutSendTurn(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s1", true, agentsessions.LiveStateProcessing)

	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	env := sendMessage(t, st, to)

	outcome := attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "s1", "wake up")
	if outcome.Delivered || outcome.Reason != "busy" {
		t.Fatalf("outcome = %+v, want Delivered=false Reason=busy", outcome)
	}
	if n := rt.sendCallCount("s1"); n != 0 {
		t.Fatalf("sendTurn called %d times while busy, want 0 (unsupported steering must fail honestly, not double-submit)", n)
	}

	deliveryID, _, _ := st.DeliveryIDForMessage(ctx, env.ID)
	rd, err := st.DeliveryStore().GetDelivery(ctx, delivery.DeliveryID(deliveryID))
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if rd.Status != delivery.DeliveryRetryScheduled && rd.Status != delivery.DeliveryLeased {
		t.Fatalf("delivery status = %q, want retry_scheduled (or leased pending the Nack's next-attempt window)", rd.Status)
	}
	if rd.NextAttemptAt.IsZero() || !rd.NextAttemptAt.After(time.Now()) {
		t.Fatalf("next_attempt_at = %v, want a future time (bounded backpressure, not an immediate re-hammer)", rd.NextAttemptAt)
	}

	// This is the real, attemptWake-driven proof of the "interrupted
	// between host_accepted and turn_submitted" boundary: busy is the
	// production condition that stops attemptWake at exactly that point,
	// so the receipt trail it leaves behind is the actual evidence, not a
	// hand-simulated stand-in for it.
	receipts, err := st.DeliveryStore().Receipts(ctx, delivery.DeliveryID(deliveryID))
	if err != nil {
		t.Fatalf("receipts: %v", err)
	}
	var sawHostAccepted bool
	for _, r := range receipts {
		if r.Stage == delivery.StageTurnSubmitted || r.Stage == delivery.StageConsumed {
			t.Fatalf("receipt %v recorded despite SendTurn never being called", r.Stage)
		}
		if r.Stage == delivery.StageHostAccepted {
			sawHostAccepted = true
		}
	}
	if !sawHostAccepted {
		t.Fatalf("receipts = %v, want host_accepted recorded (attemptWake reaches it before the busy check)", receipts)
	}
}

func TestAttemptWake_OfflineSession_NoClaimAttempted(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()

	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	env := sendMessage(t, st, to)

	// sessionID=="" is exactly what resolveActorSession returns for a
	// never-running / bound-but-offline actor -- attemptWake must not
	// burn a claim/attempt on a target it already knows is unreachable.
	outcome := attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "", "wake up")
	if outcome.Attempted || outcome.Reason != "offline" {
		t.Fatalf("outcome = %+v, want Attempted=false Reason=offline", outcome)
	}

	deliveryID, _, _ := st.DeliveryIDForMessage(ctx, env.ID)
	rd, err := st.DeliveryStore().GetDelivery(ctx, delivery.DeliveryID(deliveryID))
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if rd.Status != delivery.DeliveryPending {
		t.Fatalf("delivery status = %q, want pending (simply being offline must not burn through retries)", rd.Status)
	}
	if rd.AttemptCount != 0 {
		t.Fatalf("attempt_count = %d, want 0", rd.AttemptCount)
	}
}

// TestAttemptWake_ReceiptTrail_MatchesArchitectureVocabulary is acceptance
// #3's positive case: the full trail uses exactly the architecture's own
// stage vocabulary (persisted, host accepted, turn submitted), in order.
func TestAttemptWake_ReceiptTrail_MatchesArchitectureVocabulary(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s1", true, agentsessions.LiveStateIdle)

	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	env := sendMessage(t, st, to)
	outcome := attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "s1", "wake up")
	if !outcome.Delivered {
		t.Fatalf("outcome = %+v, want Delivered=true", outcome)
	}

	deliveryID, _, _ := st.DeliveryIDForMessage(ctx, env.ID)
	receipts, err := st.DeliveryStore().Receipts(ctx, delivery.DeliveryID(deliveryID))
	if err != nil {
		t.Fatalf("receipts: %v", err)
	}
	want := []delivery.ReceiptStage{delivery.StagePersisted, delivery.StageLeaseAcquired, delivery.StageHostAccepted, delivery.StageTurnSubmitted}
	if len(receipts) != len(want) {
		t.Fatalf("receipts = %v, want stages %v", receipts, want)
	}
	for i, w := range want {
		if receipts[i].Stage != w {
			t.Fatalf("receipts[%d].Stage = %q, want %q (full sequence: %v)", i, receipts[i].Stage, w, receipts)
		}
	}
}

// TestAttemptWake_ClaimCapturesLiveBindingGeneration is a T09 (messaging
// vNext, CW-20260906-0040) design-research fix: before this, no Tether
// call site ever set ClaimRequest.BindingGeneration, so every persisted
// Attempt recorded generation 0 regardless of which binding was actually
// live -- a delivery trace could never honestly answer "which binding
// generation served this attempt." attemptWake now resolves the live
// generation for actor-kind targets before Claim and passes it through.
func TestAttemptWake_ClaimCapturesLiveBindingGeneration(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s1", true, agentsessions.LiveStateIdle)

	target := registry.LogicalAgentBindingTarget("worker")
	// Two generations minted so the "live" one is unambiguously non-zero
	// and distinguishable from a stale/never-set value.
	if _, err := reg.LeaseBinding(ctx, target, "s0", "local", "s0", nil, "", 0); err != nil {
		t.Fatalf("lease gen 1: %v", err)
	}
	live, err := reg.LeaseBinding(ctx, target, "s1", "local", "s1", nil, "", 0)
	if err != nil {
		t.Fatalf("lease gen 2: %v", err)
	}

	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	env := sendMessage(t, st, to)
	outcome := attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "s1", "wake up")
	if !outcome.Delivered {
		t.Fatalf("outcome = %+v, want Delivered=true", outcome)
	}

	deliveryID, _, _ := st.DeliveryIDForMessage(ctx, env.ID)
	attempts, err := st.DeliveryStore().Attempts(ctx, delivery.DeliveryID(deliveryID))
	if err != nil {
		t.Fatalf("attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts = %+v, want exactly 1", attempts)
	}
	if attempts[0].BindingGeneration != live.Generation {
		t.Fatalf("attempt.BindingGeneration = %d, want the live generation %d", attempts[0].BindingGeneration, live.Generation)
	}
}

// TestAttemptWake_ExplicitSessionTarget_NoBindingGenerationRecorded
// verifies the field stays honestly zero for an exact-session-addressed
// wake, since a pinned session address has no binding generation
// concept at all (T06: "an exact session address is pinned and never
// subject to actor rebinding") -- recording a fabricated non-zero value
// here would misrepresent the trace.
func TestAttemptWake_ExplicitSessionTarget_NoBindingGenerationRecorded(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s1", true, agentsessions.LiveStateIdle)

	to := messaging.Address{Kind: messaging.KindSession, Authority: "test", ID: "s1"}
	env := sendMessage(t, st, to)
	outcome := attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "s1", "wake up")
	if !outcome.Delivered {
		t.Fatalf("outcome = %+v, want Delivered=true", outcome)
	}

	deliveryID, _, _ := st.DeliveryIDForMessage(ctx, env.ID)
	attempts, err := st.DeliveryStore().Attempts(ctx, delivery.DeliveryID(deliveryID))
	if err != nil {
		t.Fatalf("attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].BindingGeneration != 0 {
		t.Fatalf("attempts = %+v, want exactly 1 with BindingGeneration=0", attempts)
	}
}

// ─── Crash-boundary evidence ────────────────────────────────────────────────
//
// TestAttemptWake_BusySession_NacksWithoutSendTurn above is the real,
// attemptWake-driven proof for the claim/host-accepted/turn-submitted
// boundary attemptWake can actually be interrupted at through its own
// control flow (the busy check). The two tests below cover boundaries
// attemptWake's current seam gives no way to interrupt mid-function (Claim
// and Ack(host_accepted) are unconditional, back-to-back calls with no
// injectable gap between them; likewise Ack(turn_submitted) always
// immediately follows a successful SendTurn) -- calling attemptWake itself
// could not exercise "the process died between these two calls." Instead
// these drive the delivery core through the EXACT sequence attemptWake
// performs and verify the invariant attemptWake's own correctness quietly
// depends on: that go-messaging never records a stage receipt, or marks a
// delivery/message consumed, ahead of the call that's actually supposed to
// cause it. If that invariant ever broke, attemptWake would start reporting
// stages that hadn't really happened, with no test above catching it.

func TestDeliveryCoreInvariant_NoReceiptOrConsumptionAheadOfItsCausingCall_AfterClaim(t *testing.T) {
	st, _ := newWakeHarness(t)
	ctx := context.Background()

	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	env := sendMessage(t, st, to)
	deliveryID, _, _ := st.DeliveryIDForMessage(ctx, env.ID)

	// Simulate a crash immediately after Claim -- the host_accepted Ack
	// this attempt would otherwise make never runs.
	if _, err := st.DeliveryStore().Claim(ctx, delivery.ClaimRequest{
		DeliveryID: delivery.DeliveryID(deliveryID), Holder: "s1", LeaseDuration: 30 * time.Second, Nowait: true,
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	receipts, err := st.DeliveryStore().Receipts(ctx, delivery.DeliveryID(deliveryID))
	if err != nil {
		t.Fatalf("receipts: %v", err)
	}
	for _, r := range receipts {
		if r.Stage == delivery.StageHostAccepted || r.Stage == delivery.StageTurnSubmitted {
			t.Fatalf("receipt %v recorded despite the crash happening before it -- falsely acknowledged progress", r.Stage)
		}
	}
	rd, err := st.DeliveryStore().GetDelivery(ctx, delivery.DeliveryID(deliveryID))
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if rd.Status != delivery.DeliveryLeased {
		t.Fatalf("delivery status = %q, want leased (not terminal -- the lease will expire and become reclaimable, replayable evidence intact)", rd.Status)
	}
}

func TestDeliveryCoreInvariant_NoReceiptOrConsumptionAheadOfItsCausingCall_AfterHostAccepted(t *testing.T) {
	st, _ := newWakeHarness(t)
	ctx := context.Background()

	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	env := sendMessage(t, st, to)
	deliveryID, _, _ := st.DeliveryIDForMessage(ctx, env.ID)

	claim, err := st.DeliveryStore().Claim(ctx, delivery.ClaimRequest{
		DeliveryID: delivery.DeliveryID(deliveryID), Holder: "s1", LeaseDuration: 30 * time.Second, Nowait: true,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	lease := delivery.LeaseRef{DeliveryID: claim.Attempt.DeliveryID, AttemptID: claim.Attempt.ID, LeaseToken: claim.Attempt.LeaseToken, BindingGeneration: claim.Attempt.BindingGeneration}
	if _, _, err := st.DeliveryStore().Ack(ctx, delivery.AckRequest{Lease: lease, Stage: delivery.StageHostAccepted}); err != nil {
		t.Fatalf("ack host_accepted: %v", err)
	}
	// Simulate a crash here -- SendTurn/Ack(turn_submitted) never runs.

	receipts, err := st.DeliveryStore().Receipts(ctx, delivery.DeliveryID(deliveryID))
	if err != nil {
		t.Fatalf("receipts: %v", err)
	}
	if len(receipts) == 0 || receipts[len(receipts)-1].Stage != delivery.StageHostAccepted {
		t.Fatalf("receipts = %v, want to end at host_accepted (the last real stage reached)", receipts)
	}
	for _, r := range receipts {
		if r.Stage == delivery.StageTurnSubmitted || r.Stage == delivery.StageConsumed {
			t.Fatalf("receipt %v recorded despite the crash happening before it", r.Stage)
		}
	}
	msg, err := st.MessagingStore().Get(ctx, env.ID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if msg.ConsumedAt != nil {
		t.Fatalf("message.consumed_at = %v, want nil -- host_accepted must never imply consumption", msg.ConsumedAt)
	}
}

// TestDeliveryCoreInvariant_NoReceiptOrConsumptionAheadOfItsCausingCall_AfterTurnSubmitted
// documents the one honest, narrow at-least-once risk this design accepts:
// SendTurn can succeed and the model can genuinely receive the turn, and
// THEN a crash before Ack(turn_submitted)/Consume means a later retry may
// submit the same wake text again. That is "duplicate model execution is
// not overpromised" made concrete -- the guarantee this test protects is
// the one that matters: even in that exact window, the message is never
// falsely marked consumed, so a human/operator inspecting the trail sees
// an honest "turn was submitted, no consumption receipt yet" state, not a
// false "fully handled" one. Like the two invariant tests above, this
// drives the delivery core directly (attemptWake's own SendTurn ->
// Ack(turn_submitted) sequence has no injectable gap to interrupt): it
// verifies go-messaging's behavior at this exact point, which attemptWake
// relies on rather than re-derives.
func TestDeliveryCoreInvariant_NoReceiptOrConsumptionAheadOfItsCausingCall_AfterTurnSubmitted(t *testing.T) {
	st, _ := newWakeHarness(t)
	ctx := context.Background()

	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	env := sendMessage(t, st, to)
	deliveryID, _, _ := st.DeliveryIDForMessage(ctx, env.ID)

	claim, err := st.DeliveryStore().Claim(ctx, delivery.ClaimRequest{
		DeliveryID: delivery.DeliveryID(deliveryID), Holder: "s1", LeaseDuration: 30 * time.Second, Nowait: true,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	lease := delivery.LeaseRef{DeliveryID: claim.Attempt.DeliveryID, AttemptID: claim.Attempt.ID, LeaseToken: claim.Attempt.LeaseToken, BindingGeneration: claim.Attempt.BindingGeneration}
	if _, _, err := st.DeliveryStore().Ack(ctx, delivery.AckRequest{Lease: lease, Stage: delivery.StageHostAccepted}); err != nil {
		t.Fatalf("ack host_accepted: %v", err)
	}
	// SendTurn "succeeds" here (out of band -- this test only exercises
	// delivery-core state, matching attemptWake's own sequence).
	if _, _, err := st.DeliveryStore().Ack(ctx, delivery.AckRequest{Lease: lease, Stage: delivery.StageTurnSubmitted}); err != nil {
		t.Fatalf("ack turn_submitted: %v", err)
	}
	// Simulate a crash here -- Consume never runs.

	rd, err := st.DeliveryStore().GetDelivery(ctx, delivery.DeliveryID(deliveryID))
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if rd.Status == delivery.DeliveryDelivered {
		t.Fatalf("delivery status = delivered, want NOT delivered -- turn_submitted alone must never be conflated with actual consumption")
	}
	msg, err := st.MessagingStore().Get(ctx, env.ID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if msg.ConsumedAt != nil {
		t.Fatalf("message.consumed_at = %v, want nil", msg.ConsumedAt)
	}
}

// ─── Steering failure honesty + no second local owner ──────────────────────

func TestAttemptWake_TurnSubmitFails_NacksAndNeverTriesDifferentSession(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	rt.setAlive("s2", true, agentsessions.LiveStateIdle) // a second, healthy candidate that must NEVER be tried
	rt.setSendErr("s1", errUnsupportedSteering)

	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	env := sendMessage(t, st, to)

	outcome := attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "s1", "wake up")
	if outcome.Delivered || outcome.Reason != "turn-submit-failed" || outcome.Detail == "" {
		t.Fatalf("outcome = %+v, want Delivered=false Reason=turn-submit-failed with a Detail", outcome)
	}
	if n := rt.sendCallCount("s1"); n != 1 {
		t.Fatalf("sendTurn(s1) called %d times, want exactly 1", n)
	}
	if n := rt.sendCallCount("s2"); n != 0 {
		t.Fatalf("sendTurn(s2) called %d times, want 0 -- a failed selected host must never silently spawn/try a second local owner", n)
	}
}

var errUnsupportedSteering = errors.New("provider does not support mid-turn steering")

// TestAttemptWake_StaleGenerationRace_AbortsBeforeSendTurn is the explicit
// stale-generation race: sessionID was resolved a moment ago, but a NEW
// binding generation now points somewhere else by the time attemptWake
// actually runs. It must abort before SendTurn, not redirect within this
// call -- the new owner's own future resolution picks the retried delivery
// up on its own terms.
func TestAttemptWake_StaleGenerationRace_AbortsBeforeSendTurn(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s-old", true, agentsessions.LiveStateIdle)
	rt.setAlive("s-new", true, agentsessions.LiveStateIdle)

	target := registry.LogicalAgentBindingTarget("worker")
	if _, err := reg.LeaseBinding(ctx, target, "s-old", "local", "s-old", nil, "", 0); err != nil {
		t.Fatalf("lease s-old: %v", err)
	}

	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	env := sendMessage(t, st, to)

	// A resolution happened a moment ago and returned s-old. Before the
	// wake attempt runs, ownership moves.
	if _, err := reg.LeaseBinding(ctx, target, "s-new", "local", "s-new", nil, "", 0); err != nil {
		t.Fatalf("lease s-new: %v", err)
	}

	outcome := attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "s-old", "wake up")
	if outcome.Delivered || outcome.Reason != "stale-generation" {
		t.Fatalf("outcome = %+v, want Delivered=false Reason=stale-generation", outcome)
	}
	if n := rt.sendCallCount("s-old"); n != 0 {
		t.Fatalf("sendTurn(s-old) called %d times, want 0", n)
	}
	if n := rt.sendCallCount("s-new"); n != 0 {
		t.Fatalf("sendTurn(s-new) called %d times, want 0 -- never redirect within the same call", n)
	}
}

func TestAttemptWake_ClaimUnavailable_ConcurrentAttemptNoDuplicate(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s1", true, agentsessions.LiveStateIdle)

	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	env := sendMessage(t, st, to)
	deliveryID, _, _ := st.DeliveryIDForMessage(ctx, env.ID)

	// A concurrent attempt (another notify call, or the sweep) already
	// holds the claim.
	if _, err := st.DeliveryStore().Claim(ctx, delivery.ClaimRequest{
		DeliveryID: delivery.DeliveryID(deliveryID), Holder: "someone-else", LeaseDuration: 30 * time.Second, Nowait: true,
	}); err != nil {
		t.Fatalf("prior claim: %v", err)
	}

	outcome := attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "s1", "wake up")
	if outcome.Delivered || outcome.Reason != "claim-unavailable" {
		t.Fatalf("outcome = %+v, want Delivered=false Reason=claim-unavailable", outcome)
	}
	if n := rt.sendCallCount("s1"); n != 0 {
		t.Fatalf("sendTurn called %d times, want 0 (no duplicate wake for an already-claimed delivery)", n)
	}
}

// ─── RunWakeSweep: the shared pump retries what a synchronous attempt
//     couldn't finish, using the exact same AttemptWake path.

// TestRunWakeSweep_ConsumedDeliveryIsNeverRewoken is a remaining instance
// of CW-20260907-0033, found by independent review: attemptWake claims a
// delivery and Acks it through StageTurnSubmitted, then deliberately
// leaves the lease open "awaiting consumption" (Consume is what's supposed
// to Ack it the rest of the way to Consumed/Delivered). Before the fix,
// Consume's own receipt recording always tried a brand-new Claim, which
// go-messaging correctly refuses while that lease is still active
// (ErrAlreadyClaimed) -- so the old lease was simply left to expire,
// delivery-core's own release-expired-leases logic made the delivery
// retry-eligible again, and the next sweep woke an already-fully-consumed
// message a second time. This reproduces on the ordinary wake -> Consume
// -> (eventual) lease expiry path -- no crash required.
func TestRunWakeSweep_ConsumedDeliveryIsNeverRewoken(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s1", true, agentsessions.LiveStateIdle)

	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	env := sendMessage(t, st, to)
	if _, err := reg.LeaseBinding(ctx, registry.LogicalAgentBindingTarget("worker"), "s1", "local", "s1", nil, "", 0); err != nil {
		t.Fatalf("lease: %v", err)
	}

	wake := attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "s1", "wake up")
	if !wake.Delivered {
		t.Fatalf("initial wake: %+v", wake)
	}
	if err := st.MessagingStore().Consume(ctx, env.ID, to); err != nil {
		t.Fatalf("consume: %v", err)
	}
	deliveryID, ok, err := st.DeliveryIDForMessage(ctx, env.ID)
	if err != nil || !ok {
		t.Fatalf("delivery id: ok=%v err=%v", ok, err)
	}
	del, err := st.DeliveryStore().GetDelivery(ctx, delivery.DeliveryID(deliveryID))
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if del.Status != delivery.DeliveryDelivered {
		t.Fatalf("delivery status after wake+consume = %q, want delivered -- the still-open wake lease was not reused/completed", del.Status)
	}

	// Advance the (already-terminal) delivery's stale lease_expires_at
	// directly -- this fixture has no fake-clock seam into the delivery
	// core, matching TestRunWakeSweep_RetriesBusyDelivery_ThenDelivers's
	// own note above. A terminal (Delivered) delivery's lease fields are
	// already cleared by Ack(StageConsumed), so this column write is
	// inert for a correctly-fixed delivery; it only matters as a guard
	// against a regression that leaves a stale, non-cleared expiry behind.
	if _, err := st.DB().ExecContext(ctx, "UPDATE messaging_deliveries SET lease_expires_at=? WHERE id=?",
		time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), deliveryID); err != nil {
		t.Fatalf("advance lease_expires_at: %v", err)
	}

	attempted, err := runWakeSweep(ctx, st, reg, rt.seam())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if attempted != 0 || rt.sendCallCount("s1") != 1 {
		t.Fatalf("an already-consumed message was woken again: sweep attempted=%d, total sendTurn calls=%d, want attempted=0 calls=1", attempted, rt.sendCallCount("s1"))
	}
}

// TestAttemptWake_ConcurrentConsumeDuringBusyCheckStaysDelivered is the
// consumer-side regression test for a go-messaging bug an independent
// review found (fixed in go-messaging v0.5.1, this repo now pins that
// release): attemptWake's own lease can legitimately be completed by a
// CONCURRENT Consume call while attemptWake itself is still mid-flight
// (specifically, between its host_accepted Ack and its busy/offline/
// send-error check) -- this is not a crash or a hijack, just two
// legitimate operations racing on the same, correctly-owned lease. When
// attemptWake then observes "busy" and Nacks that lease, the Nack must be
// a no-op against an already-Delivered attempt, not reverse it back to
// retry_scheduled -- otherwise the next wake sweep re-delivers content the
// recipient already consumed.
//
// Before go-messaging v0.5.1, Nack's completed-idempotent lease-fencing
// allowance let a late Nack through against an already-StageConsumed
// attempt (its early no-op guard checked only Failed/DeadLettered), so
// this reproduced with no crash at all. This test drives real Claim/
// Consume/Ack/Nack/sweep production code, only substituting the runtime
// health seam to inject the interleaving deterministically.
func TestAttemptWake_ConcurrentConsumeDuringBusyCheckStaysDelivered(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s1", true, agentsessions.LiveStateIdle)

	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	env := sendMessage(t, st, to)
	if _, err := reg.LeaseBinding(ctx, registry.LogicalAgentBindingTarget("worker"), "s1", "local", "s1", nil, "", 0); err != nil {
		t.Fatalf("lease: %v", err)
	}
	deliveryID, ok, err := st.DeliveryIDForMessage(ctx, env.ID)
	if err != nil || !ok {
		t.Fatalf("delivery id: ok=%v err=%v", ok, err)
	}

	// Wrap the health seam: the FIRST call (attemptWake's busy/offline
	// check, which runs after Claim/marker/host_accepted but before
	// SendTurn) triggers a real, concurrent Consume for the SAME message
	// first, then reports the session as busy -- reproducing "a legitimate
	// consumer finished this exact lease while attemptWake was still
	// mid-flight."
	seam := rt.seam()
	originalHealth := seam.health
	seam.health = func(sessionID string) (api.RuntimeHealthResult, bool) {
		if err := st.MessagingStore().Consume(ctx, env.ID, to); err != nil {
			t.Fatalf("concurrent consume: %v", err)
		}
		mid, err := st.DeliveryStore().GetDelivery(ctx, delivery.DeliveryID(deliveryID))
		if err != nil {
			t.Fatalf("get delivery mid-wake: %v", err)
		}
		if mid.Status != delivery.DeliveryDelivered {
			t.Fatalf("concurrent consume failed to finish the marked lease: %s", mid.Status)
		}
		result, exists := originalHealth(sessionID)
		result.Health.State = agentsessions.LiveStateProcessing
		return result, exists
	}

	outcome := attemptWake(ctx, st, reg, seam, env.ID, to, "s1", "wake up")
	if outcome.Reason != "busy" {
		t.Fatalf("outcome = %+v, want Reason=busy", outcome)
	}

	after, err := st.DeliveryStore().GetDelivery(ctx, delivery.DeliveryID(deliveryID))
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if after.Status != delivery.DeliveryDelivered {
		t.Fatalf("status = %q after wake's late Nack, want delivered -- a late Nack reversed a completed Consume", after.Status)
	}

	// Force the (already-terminal) delivery ready for a sweep, same
	// technique as TestRunWakeSweep_ConsumedDeliveryIsNeverRewoken.
	if _, err := st.DB().ExecContext(ctx, "UPDATE messaging_deliveries SET next_attempt_at=? WHERE id=?",
		time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), deliveryID); err != nil {
		t.Fatalf("advance next_attempt_at: %v", err)
	}
	attempted, err := runWakeSweep(ctx, st, reg, rt.seam())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if attempted != 0 || rt.sendCallCount("s1") != 0 {
		t.Fatalf("an already-consumed message was woken again: sweep attempted=%d, total sendTurn calls=%d, want attempted=0 calls=0", attempted, rt.sendCallCount("s1"))
	}
}

func TestRunWakeSweep_RetriesBusyDelivery_ThenDelivers(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s1", true, agentsessions.LiveStateProcessing)

	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	env := sendMessage(t, st, to)
	if _, err := reg.LeaseBinding(ctx, registry.LogicalAgentBindingTarget("worker"), "s1", "local", "s1", nil, "", 0); err != nil {
		t.Fatalf("lease: %v", err)
	}

	// First attempt: busy, Nacked for retry.
	first := attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "s1", "wake up")
	if first.Reason != "busy" {
		t.Fatalf("first outcome = %+v, want Reason=busy", first)
	}

	// A sweep run immediately after must not re-attempt (still within the
	// busy backoff window) -- proves the pump respects ReadyOnly rather
	// than hammering.
	n, err := runWakeSweep(ctx, st, reg, rt.seam())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("sweep attempted %d deliveries immediately after a busy Nack, want 0 (bounded backpressure)", n)
	}

	// The session goes idle. Wait out the busy backoff window (this test
	// does not have a fake-clock seam into the delivery core's own
	// next_attempt_at bookkeeping -- see the file doc comment) and sweep
	// again; the delivery should now be ready.
	rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	time.Sleep(wakeBusyRetryBackoff + 250*time.Millisecond)

	n, err = runWakeSweep(ctx, st, reg, rt.seam())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("sweep attempted %d deliveries once ready and idle, want 1", n)
	}
	if rt.sendCallCount("s1") != 1 {
		t.Fatalf("sendTurn called %d times, want exactly 1 (delivered on the retry, no duplicate)", rt.sendCallCount("s1"))
	}
}
