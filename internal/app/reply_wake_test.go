package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/store"
)

// CW-20261117-0002: replies reach the asker, wakes are coalesced, busy seats
// are not given up on, and stranded replies are backfilled.

func (r *triageRig) sendFrom(from, to string, kind messaging.Kind, inReplyTo string, payload any) messaging.Envelope {
	r.t.Helper()
	raw, _ := json.Marshal(payload)
	f, _ := messaging.ParseURN(from)
	dst, _ := messaging.ParseURN(to)
	env, err := r.st.MessagingStore().Send(context.Background(), messaging.Envelope{Kind: kind, From: f, To: dst, InReplyTo: inReplyTo, Payload: raw})
	if err != nil {
		r.t.Fatal(err)
	}
	return env
}

func (r *triageRig) lastText() string {
	r.rt.mu.Lock()
	defer r.rt.mu.Unlock()
	if len(r.rt.sendCalls) == 0 {
		return ""
	}
	return r.rt.sendCalls[len(r.rt.sendCalls)-1].Text
}

// An untyped response to mail the seat sent wakes it (it was fyi, and fyi
// never woke a hosted seat).
func TestReplyToOwnMailWakesTheAsker(t *testing.T) {
	r := newTriageRig(t)
	ask := r.sendFrom(r.to.URN(), peer, messaging.MsgKindRequest, "", map[string]string{"subject": "approve?"})
	reply := r.sendFrom(peer, r.to.URN(), messaging.MsgKindResponse, ask.ID, map[string]string{"subject": "Re: approve?", "body": "approved, build it"})
	if reason := r.wake(reply); reason != "" {
		t.Fatalf("reply wake reason %q, want a delivered wake", reason)
	}
	if n := r.rt.sendCallCount("live"); n != 1 {
		t.Fatalf("reply produced %d turns, want 1", n)
	}
	row, _, _ := r.st.TriageGet(context.Background(), reply.ID)
	if row.Class != "action" || row.Reason != "reply_to_own" {
		t.Fatalf("reply triaged %s/%s, want action/reply_to_own", row.Class, row.Reason)
	}
	// A response that answers someone else's mail is still fyi.
	other := r.sendFrom(peer, peer, messaging.MsgKindRequest, "", map[string]string{"subject": "not yours"})
	stray := r.sendFrom(peer, r.to.URN(), messaging.MsgKindResponse, other.ID, map[string]string{"body": "cc"})
	if reason := r.wake(stray); reason != "triaged-fyi" {
		t.Fatalf("stray response: %q, want triaged-fyi", reason)
	}
}

// A reply to the seat's catalog-id form (not its urn) is still its own.
func TestReplyToOwnMatchesTheSeatsAliasForm(t *testing.T) {
	r := newTriageRig(t)
	alias := "msg://agent/agent-mux/worker-alias"
	r.tri.policy = func(messaging.Address) seatPolicy {
		return seatPolicy{seat: r.to.URN(), handle: "@worker", enabled: true, interval: 3 * time.Minute, forms: []string{alias}}
	}
	ask := r.sendFrom(alias, peer, messaging.MsgKindRequest, "", map[string]string{"subject": "q"})
	reply := r.sendFrom(peer, r.to.URN(), messaging.MsgKindResponse, ask.ID, map[string]string{"body": "a"})
	if reason := r.wake(reply); reason != "" {
		t.Fatalf("alias reply: %q", reason)
	}
}

// Two actions and one fyi arriving inside the debounce window produce one wake
// turn that names all three; none of them wakes again afterwards.
func TestCoalescedWakeListsEverythingPending(t *testing.T) {
	r := newTriageRig(t)
	r.wake(r.send(peer, messaging.MsgKindRequest, "", map[string]string{"subject": "first"}, nil))
	if r.rt.sendCallCount("live") != 1 {
		t.Fatal("first action did not wake")
	}
	r.now = r.now.Add(time.Minute)
	b := r.send(peer, messaging.MsgKindRequest, "", map[string]string{"subject": "second"}, nil)
	c := r.send(peer, messaging.MsgKindEscalation, "", map[string]string{"subject": "third"}, nil)
	f := r.send(peer, messaging.MsgKindNotice, "", map[string]string{"subject": "fourth fyi"}, nil)
	for _, env := range []messaging.Envelope{b, c, f} {
		r.wake(env)
	}
	if r.rt.sendCallCount("live") != 1 {
		t.Fatalf("mail inside the window woke the seat: %d turns", r.rt.sendCallCount("live"))
	}
	r.now = r.now.Add(5 * time.Minute)
	for i := 0; i < 3; i++ {
		if _, err := runWakeSweep(context.Background(), r.st, nil, r.seam, &r.park); err != nil {
			t.Fatal(err)
		}
	}
	if n := r.rt.sendCallCount("live"); n != 2 {
		t.Fatalf("debounced mail produced %d wake turns after the first, want 1", n-1)
	}
	text := r.lastText()
	for _, env := range []messaging.Envelope{b, c, f} {
		if !strings.Contains(text, env.ID) {
			t.Fatalf("coalesced wake does not name %s:\n%s", env.ID, text)
		}
	}
	if !strings.Contains(text, "Reply to anything that asked you something") {
		t.Fatalf("wake text lacks the new guidance:\n%s", text)
	}
	for _, env := range []messaging.Envelope{b, c, f} {
		row, _, _ := r.st.TriageGet(context.Background(), env.ID)
		if row.State == store.TriagePending {
			t.Fatalf("%s still pending after the coalesced wake", env.ID)
		}
	}
}

// A seat busy far longer than the old 12-attempt give-up still gets the wake
// when its turn ends.
func TestBusySeatGetsTheWakeOnTurnEnd(t *testing.T) {
	r := newTriageRig(t)
	prev := wakeBusyRetryBackoff
	wakeBusyRetryBackoff = time.Hour // only a re-arm makes the delivery ready
	t.Cleanup(func() { wakeBusyRetryBackoff = prev })
	svc := &Service{Store: r.st}
	r.seam.busy = svc.wakeRearm.add
	r.rt.setAlive("live", true, agentsessions.LiveStateProcessing)

	a := r.send(peer, messaging.MsgKindRequest, "", map[string]string{"subject": "do it"}, nil)
	if reason := r.wake(a); reason != "busy" {
		t.Fatalf("first attempt: %q, want busy", reason)
	}
	// Twenty turn ends while still busy (well past wakeMaxAttempts): each
	// re-arms the wake and finds the session busy again.
	for i := 0; i < 20; i++ {
		svc.rearmBusyWakes("live")
		if _, err := runWakeSweep(context.Background(), r.st, nil, r.seam, &r.park); err != nil {
			t.Fatal(err)
		}
	}
	if r.rt.sendCallCount("live") != 0 {
		t.Fatal("a busy session was handed a turn")
	}
	// Without a turn end the backoff holds it (capped, never the give-up bound).
	r.rt.setAlive("live", true, agentsessions.LiveStateIdle)
	if _, err := runWakeSweep(context.Background(), r.st, nil, r.seam, &r.park); err != nil {
		t.Fatal(err)
	}
	if r.rt.sendCallCount("live") != 0 {
		t.Fatal("the wake ran before the turn end re-armed it")
	}
	svc.rearmBusyWakes("live")
	if _, err := runWakeSweep(context.Background(), r.st, nil, r.seam, &r.park); err != nil {
		t.Fatal(err)
	}
	if n := r.rt.sendCallCount("live"); n != 1 {
		t.Fatalf("turn end delivered %d wakes, want 1", n)
	}
}

func TestWakeBusyDelayNeverGivesUp(t *testing.T) {
	for _, attempt := range []int{1, 12, 13, 50, 500} {
		if d := wakeBusyDelay(attempt); d <= 0 || d > wakeBusyRetryCap*12/10 {
			t.Fatalf("attempt %d: busy delay %v outside (0, %v]", attempt, d, wakeBusyRetryCap*12/10)
		}
	}
}

// The backfill moves pending fyi replies to the seat's own mail to action,
// once; the sweep then wakes the seat once with a digest of them.
func TestReplyToOwnBackfill(t *testing.T) {
	r := newTriageRig(t)
	ctx := context.Background()
	stranded := func(parentFrom string) messaging.Envelope {
		ask := r.sendFrom(parentFrom, peer, messaging.MsgKindRequest, "", map[string]string{"subject": "ask"})
		reply := r.sendFrom(peer, r.to.URN(), messaging.MsgKindResponse, ask.ID, map[string]string{"subject": "Re: ask"})
		// As the pre-fix triage filed it.
		if _, _, err := r.st.TriageRecord(ctx, store.TriageRow{MessageID: reply.ID, Recipient: r.to.URN(), Seat: r.to.URN(), Class: "fyi", Reason: "transport_fyi", CreatedAt: r.now}); err != nil {
			t.Fatal(err)
		}
		return reply
	}
	one, two := stranded(r.to.URN()), stranded(r.to.URN())
	notOwn := stranded("msg://agent/agent-mux/someone-else")

	examined, moved, seats, err := r.tri.backfillReplyToOwn(ctx)
	if err != nil || examined != 3 || moved != 2 || seats[r.to.URN()] != 2 {
		t.Fatalf("backfill: examined %d moved %d seats %v err %v", examined, moved, seats, err)
	}
	if _, moved, _, _ := r.tri.backfillReplyToOwn(ctx); moved != 0 {
		t.Fatalf("rerun moved %d, want 0", moved)
	}
	if row, _, _ := r.st.TriageGet(ctx, notOwn.ID); row.Class != "fyi" {
		t.Fatalf("a reply to someone else's mail moved to %s", row.Class)
	}
	for i := 0; i < 3; i++ {
		if _, err := runWakeSweep(ctx, r.st, nil, r.seam, &r.park); err != nil {
			t.Fatal(err)
		}
	}
	if n := r.rt.sendCallCount("live"); n != 1 {
		t.Fatalf("backfill produced %d wakes for one seat, want 1", n)
	}
	text := r.lastText()
	for _, env := range []messaging.Envelope{one, two} {
		if !strings.Contains(text, env.ID) {
			t.Fatalf("digest wake does not name %s:\n%s", env.ID, text)
		}
	}
}
