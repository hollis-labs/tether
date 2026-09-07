package e2e

// capability_honesty_test.go — T11 "provider-capability honesty"
// scenario. Per this session's own design research: capabilities are
// self-asserted under this codebase's whole same-host trust model (ADR
// 0045) and the public lease API only accepts the literal value
// ["pull-only"] (internal/api/bindings_test.go's
// TestBindingLease_RejectsUnsupportedCapabilities) -- there is no
// mechanism anywhere that could "catch a binding lying" about a broader
// capability, because none exists to lie about. The meaningful,
// buildable property is therefore NOT "detect a false claim" but "the
// system stays safe when a declared capability and actual behavior
// diverge": a binding that declares pull-only and then never actually
// pulls its mailbox must never cause a message to be force-pushed,
// silently dropped, or corrupted -- it just stays durably queued for
// whoever eventually does pull it, honest claim or not.

import (
	"testing"
	"time"
)

func TestCapabilityHonesty_PullOnlyBindingThatNeverPulls_MessageStaysSafelyQueued(t *testing.T) {
	d := StartFixtureDaemon(t)
	c := d.Client()

	agentURN := registerAgent(t, c, "e2e-pull-only-liar")
	sender := registerAgent(t, c, "e2e-pull-only-sender")

	// Declares pull-only, then this test deliberately never calls claim
	// or consume as that actor -- simulating a binding that claimed a
	// capability (or simply exists) but never behaves accordingly.
	if _, err := c.Bindings().Lease(ctx(), agentURN, "sess-liar", "host-1", "attempt-1", []string{"pull-only"}, 60); err != nil {
		t.Fatalf("lease pull-only: %v", err)
	}

	sent, err := c.MessageSend(ctx(), sendNotice(t, sender, agentURN, "message for a binding that will never pull"))
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	// Give the daemon's background wake-sweep (5s interval,
	// internal/daemon/server.go's wakeSweepInterval) more than a full
	// cycle to run -- proving it does not crash, corrupt, or force-push
	// against a declared-but-inactive pull-only target.
	time.Sleep(6 * time.Second)

	if err := c.Ping(t.Context()); err != nil {
		t.Fatalf("daemon unresponsive after a sweep cycle against an unpulled pull-only binding: %v", err)
	}

	trace, err := c.MessageTrace(ctx(), sent.ID)
	if err != nil {
		t.Fatalf("trace: %v", err)
	}
	if trace.Status != "pending" {
		t.Fatalf("status = %q, want pending -- an unhonored pull-only declaration must never cause an auto-delivery", trace.Status)
	}

	// The message is still safely there for a real, honest pull.
	if err := c.MessageConsume(ctx(), sent.ID, agentURN); err != nil {
		t.Fatalf("consume after the sweep cycle: %v", err)
	}
}
