package app

import (
	"context"
	"testing"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	messaging "github.com/hollis-labs/substrate/mesh/messaging"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

// An interrupting reply a dead daemon had only reserved is settled
// undeliverable/interrupt_unconfirmed, and its caller is told to resubmit. The old
// Idempotency-Key still names that reply, so resubmitting with it returns the dead
// reply (duplicate, undeliverable) and delivers nothing; a new key makes a new
// reply. The README says so; this pins it.
func TestResubmittingAnUnconfirmedInterruptNeedsANewIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	h := newReplyHarness(t)
	h.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	parent := h.routed("s1")
	caller := identity.Principal{ID: "msg://user/local/chris", Kind: "user"}
	req := api.RoutingReplyRequest{ParentID: parent.ID, Body: "stop and read this", Interrupt: true, IdempotencyKey: "k1", Verified: true, Caller: caller}

	// A previous daemon reserved the reply and died before its cancel finished.
	dead, _, err := h.st.CreateRoutingReply(ctx, store.NewRoutingReply{From: messaging.Address{Kind: messaging.KindUser, Authority: "local", ID: "chris"},
		ParentID: parent.ID, Body: req.Body, TargetSessionID: "s1", Actor: caller.ID, Interrupt: true, Pending: true, IdempotencyKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.waitState(dead.ReplyID, store.RoutingReplyUndeliverable); got.Reason != ReplyReasonInterruptUnconfirmed {
		t.Fatalf("reply = %+v", got)
	}

	same, err := h.svc.SubmitRoutingReply(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !same.Duplicate || same.ReplyID != dead.ReplyID || same.State != string(store.RoutingReplyUndeliverable) {
		t.Fatalf("the same key = %+v, want the dead reply back as a duplicate", same)
	}
	settleQuiet()
	if h.rt.sendCallCount("s1") != 0 {
		t.Fatal("the dead reply was delivered")
	}

	req.IdempotencyKey = "k2"
	fresh, err := h.svc.SubmitRoutingReply(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Duplicate || fresh.ReplyID == dead.ReplyID {
		t.Fatalf("a new key = %+v, want a new reply", fresh)
	}
	h.waitState(fresh.ReplyID, store.RoutingReplyDelivered)
}
