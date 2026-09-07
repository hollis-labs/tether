package e2e

// lost_notifications_test.go — T11 "lost notifications" scenario. Unit
// coverage already exists for the wake-sweep retry mechanism itself
// (internal/app/wake_test.go, internal/daemon/wake_sweep_test.go). This
// promotes the core guarantee to a real running daemon process without
// launching a real provider/agent (out of bounds for this task): a
// notified message to a recipient with NO live binding at all -- nothing
// for the wake attempt or the background sweep to push to -- must never
// be silently dropped. It survives across multiple real wake-sweep
// cycles (5s interval, internal/daemon/server.go's wakeSweepInterval)
// and remains fully and correctly claimable afterward.

import (
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/client"
)

func TestLostNotifications_NotifiedMessageToUnreachableTargetSurvivesMultipleSweepCycles(t *testing.T) {
	d := StartFixtureDaemon(t)
	c := d.Client()

	sender := registerAgent(t, c, "e2e-notif-sender")
	// Registered but never leased any binding -- there is nothing for a
	// wake attempt or the background sweep to push to.
	recipient := registerAgent(t, c, "e2e-notif-unreachable")

	notifyResult, err := c.MessageNotify(ctx(), client.MessageNotifyRequest{
		MessageSendRequest: sendNotice(t, sender, recipient, "nobody is listening yet"),
	})
	if err != nil {
		t.Fatalf("notify: %v", err)
	}
	if notifyResult.WakeDelivered {
		t.Fatal("notify reported WakeDelivered=true against a target with no live binding -- should be an honest no-op, not a false success")
	}
	messageID := notifyResult.Message.ID

	// Cross more than two full sweep cycles.
	time.Sleep(11 * time.Second)

	if err := c.Ping(t.Context()); err != nil {
		t.Fatalf("daemon unresponsive after sweep cycles against an unreachable target: %v", err)
	}

	trace, err := c.MessageTrace(ctx(), messageID)
	if err != nil {
		t.Fatalf("trace after sweep cycles: %v", err)
	}
	if trace.Status != "pending" {
		t.Fatalf("status = %q, want still pending -- nothing should have force-delivered or dropped this message", trace.Status)
	}

	// The message is still there, intact, and consumable once someone
	// finally does show up to read it -- the actual "no lost
	// notification" guarantee.
	got, err := c.MessageGet(ctx(), messageID, recipient)
	if err != nil {
		t.Fatalf("get after sweep cycles: %v", err)
	}
	if got.From != sender || got.To != recipient {
		t.Fatalf("message From/To = %s/%s, want %s/%s (message content/addressing must be unchanged)", got.From, got.To, sender, recipient)
	}
	if err := c.MessageConsume(ctx(), messageID, recipient); err != nil {
		t.Fatalf("consume after sweep cycles: %v", err)
	}
}
