package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/store"
)

// Pending action mail whose delivery will never be swept (already
// acknowledged by an adapter, or buried in a backlog) still reaches its seat:
// one digest turn to the live idle session, once (CW-20261117-0002).
func TestSeatDigestWakesStrandedActionMail(t *testing.T) {
	r := newTriageRig(t)
	ctx := context.Background()
	r.tri.policy = func(messaging.Address) seatPolicy {
		return seatPolicy{seat: r.to.URN(), handle: "@worker", enabled: true, interval: 3 * time.Minute, agentID: "worker"}
	}
	resolve := func(_ context.Context, id string) (string, error) {
		if id != "worker" {
			t.Fatalf("resolved %q", id)
		}
		return "live", nil
	}
	ask := r.sendFrom(r.to.URN(), peer, messaging.MsgKindRequest, "", map[string]string{"subject": "q"})
	reply := r.sendFrom(peer, r.to.URN(), messaging.MsgKindResponse, ask.ID, map[string]string{"subject": "Re: q", "body": "approved"})
	// Triaged (as the gate does) but never woken.
	if _, err := r.tri.record(ctx, reply, r.tri.policy(r.to), r.now); err != nil {
		t.Fatal(err)
	}

	// Inside the grace the delivery sweep still owns it.
	if n, err := runSeatDigest(ctx, r.st, r.tri, r.seam, resolve); err != nil || n != 0 {
		t.Fatalf("inside grace: woke %d, %v", n, err)
	}
	r.now = r.now.Add(seatDigestGrace + time.Second)
	// A busy session is left for a later pass.
	r.rt.setAlive("live", true, agentsessions.LiveStateProcessing)
	if n, _ := runSeatDigest(ctx, r.st, r.tri, r.seam, resolve); n != 0 || r.rt.sendCallCount("live") != 0 {
		t.Fatal("woke a busy session")
	}
	r.rt.setAlive("live", true, agentsessions.LiveStateIdle)
	if n, err := runSeatDigest(ctx, r.st, r.tri, r.seam, resolve); err != nil || n != 1 {
		t.Fatalf("stranded reply: woke %d, %v", n, err)
	}
	if text := r.lastText(); !strings.Contains(text, reply.ID) || !strings.Contains(text, "[reply to you]") {
		t.Fatalf("digest turn:\n%s", text)
	}
	if row, _, _ := r.st.TriageGet(ctx, reply.ID); row.State != store.TriageCovered {
		t.Fatalf("state %q after the digest wake", row.State)
	}
	r.now = r.now.Add(time.Hour)
	if n, _ := runSeatDigest(ctx, r.st, r.tri, r.seam, resolve); n != 0 || r.rt.sendCallCount("live") != 1 {
		t.Fatal("the same mail woke the seat twice")
	}
}

// The seat pass keeps the per-seat action interval.
func TestSeatDigestKeepsTheInterval(t *testing.T) {
	r := newTriageRig(t)
	ctx := context.Background()
	r.tri.policy = func(messaging.Address) seatPolicy {
		return seatPolicy{seat: r.to.URN(), handle: "@worker", enabled: true, interval: 3 * time.Minute, agentID: "worker"}
	}
	env := r.send(peer, messaging.MsgKindRequest, "", map[string]string{"subject": "do"}, nil)
	if _, err := r.tri.record(ctx, env, r.tri.policy(r.to), r.now); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(seatDigestGrace + time.Second)
	_ = r.st.SetSeatLastActionWake(ctx, r.to.URN(), r.now.Add(-time.Minute))
	resolve := func(context.Context, string) (string, error) { return "live", nil }
	if n, _ := runSeatDigest(ctx, r.st, r.tri, r.seam, resolve); n != 0 {
		t.Fatal("woke inside the action interval")
	}
	r.now = r.now.Add(3 * time.Minute)
	if n, _ := runSeatDigest(ctx, r.st, r.tri, r.seam, resolve); n != 1 {
		t.Fatal("did not wake after the interval")
	}
}
