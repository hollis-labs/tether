package e2e

// consumer_nanite_test.go — T11's "Nanite durable actor/team" fixture
// consumer. Nanite itself is a separate repository this task must not
// edit; no Nanite integration code exists in this repo to reference, so
// this shape-matches the pattern T02/T06/T07 were built for: a durable
// actor identity that outlives any one session, hosted by a live
// runtime binding that can be reconnected after the host session dies,
// exchanging messages with a teammate throughout.

import (
	"testing"
)

func TestConsumer_Nanite_DurableActorSurvivesReconnectAndKeepsMessaging(t *testing.T) {
	d := StartFixtureDaemon(t)
	c := d.Client()

	// Two durable "team" members. Their URNs are the stable identity
	// Nanite would hold onto across the actor's entire lifetime,
	// independent of any one process/session.
	alice := registerAgent(t, c, "e2e-nanite-alice")
	bob := registerAgent(t, c, "e2e-nanite-bob")

	// Alice's first host session leases a binding -- "the actor is live,
	// hosted here."
	firstBinding, err := c.Bindings().Lease(ctx(), alice, "nanite-sess-1", "nanite-host-1", "attempt-1", []string{"pull-only"}, 60)
	if err != nil {
		t.Fatalf("lease first binding: %v", err)
	}

	sent1, err := c.MessageSend(ctx(), sendNotice(t, bob, alice, "hello alice, round one"))
	if err != nil {
		t.Fatalf("send round 1: %v", err)
	}
	if err := c.MessageConsume(ctx(), sent1.ID, alice); err != nil {
		t.Fatalf("alice consumes round 1: %v", err)
	}

	// The host session dies (crash, redeploy, whatever) -- Nanite
	// reconnects the SAME durable actor from a NEW host/session, which
	// means revoking the stale binding and leasing a fresh one. The
	// actor's own identity (URN) never changes.
	if err := c.Bindings().Revoke(ctx(), firstBinding.ID); err != nil {
		t.Fatalf("revoke stale binding on reconnect: %v", err)
	}
	secondBinding, err := c.Bindings().Lease(ctx(), alice, "nanite-sess-2", "nanite-host-2", "attempt-2", []string{"pull-only"}, 60)
	if err != nil {
		t.Fatalf("lease second binding on reconnect: %v", err)
	}
	if secondBinding.ID == firstBinding.ID {
		t.Fatal("reconnect produced the same binding id as before revocation")
	}

	current, err := c.Bindings().Current(ctx(), alice)
	if err != nil {
		t.Fatalf("current after reconnect: %v", err)
	}
	if current.ID != secondBinding.ID {
		t.Fatalf("current binding after reconnect = %q, want the fresh one %q", current.ID, secondBinding.ID)
	}

	// Alice's identity kept working across the reconnect: a NEW message
	// addressed to the SAME URN is deliverable and consumable exactly as
	// before.
	sent2, err := c.MessageSend(ctx(), sendNotice(t, bob, alice, "hello alice, round two, after reconnect"))
	if err != nil {
		t.Fatalf("send round 2: %v", err)
	}
	if err := c.MessageConsume(ctx(), sent2.ID, alice); err != nil {
		t.Fatalf("alice consumes round 2 after reconnect: %v", err)
	}

	// Bob's own identity is equally durable -- a reply flows back to him
	// under the same stable URN he registered with at the start.
	reply, err := c.MessageSend(ctx(), sendNotice(t, alice, bob, "got both, thanks"))
	if err != nil {
		t.Fatalf("alice replies to bob: %v", err)
	}
	if err := c.MessageConsume(ctx(), reply.ID, bob); err != nil {
		t.Fatalf("bob consumes alice's reply: %v", err)
	}

	profile, err := c.Registry().Lookup(ctx(), alice)
	if err != nil {
		t.Fatalf("lookup alice after full reconnect cycle: %v", err)
	}
	if profile.URN != alice {
		t.Fatalf("looked-up URN = %q, want %q -- identity must be exactly what it was before reconnect", profile.URN, alice)
	}
}
