package app

import (
	"context"
	"testing"

	"github.com/hollis-labs/agentkit/agentsessions"
	messaging "github.com/hollis-labs/go-messaging"

	"github.com/hollis-labs/tether/internal/store"
)

// ─── the sweep acts on a snapshot (F3) ──────────────────────────────────────

// resolveStale reads rows and then writes them, while a live drain or an
// interrupting submission can move a row in between. Each write must apply only if
// the row is exactly as it was read: otherwise the sweep abandons a reply that was
// just requeued, or settles a just-promoted interrupting reply as unconfirmed
// after its caller was told it was accepted.
func TestTheStaleSweepNeverOverwritesARowThatMovedAfterItWasRead(t *testing.T) {
	ctx := context.Background()
	read := func(h *replyHarness, id string) store.RoutingReply { return h.state(id) }

	t.Run("a delivering row requeued after the read, session running", func(t *testing.T) {
		h := newReplyHarness(t)
		h.rt.setAlive("s1", true, agentsessions.LiveStateProcessing)
		r := h.queued("s1", "x", false)
		if ok, _ := h.st.ClaimRoutingReply(ctx, r.ReplyID); !ok {
			t.Fatal("claim")
		}
		snapshot := read(h, r.ReplyID)
		if err := h.st.RequeueRoutingReply(ctx, r.ReplyID, store.RoutingReplyRequeue{Reason: ReplyReasonWaitingForIdle}); err != nil {
			t.Fatal(err)
		}
		h.d.resolveStaleDelivering(snapshot)
		if got := h.state(r.ReplyID); got.State != store.RoutingReplyQueued || got.Reason != ReplyReasonWaitingForIdle {
			t.Fatalf("the sweep settled a row that moved after it read it: %+v", got)
		}
	})
	t.Run("a delivering row claimed again after the read, session gone", func(t *testing.T) {
		h := newReplyHarness(t)
		r := h.queued("s-gone", "x", false)
		if ok, _ := h.st.ClaimRoutingReply(ctx, r.ReplyID); !ok {
			t.Fatal("claim")
		}
		snapshot := read(h, r.ReplyID)
		if err := h.st.RequeueRoutingReply(ctx, r.ReplyID, store.RoutingReplyRequeue{Reason: ReplyReasonSubmitFailed}); err != nil {
			t.Fatal(err)
		}
		if ok, _ := h.st.ClaimRoutingReply(ctx, r.ReplyID); !ok { // delivering again, but not the version the sweep read
			t.Fatal("claim again")
		}
		h.d.resolveStaleDelivering(snapshot)
		if got := h.state(r.ReplyID); got.State != store.RoutingReplyDelivering || got.Reason != ReplyReasonSubmitFailed {
			t.Fatalf("the sweep requeued a row that a live drain owns again: %+v", got)
		}
	})
	t.Run("a pending reservation promoted after the read", func(t *testing.T) {
		h := newReplyHarness(t)
		r, _, err := h.st.CreateRoutingReply(ctx, store.NewRoutingReply{
			From: messaging.Address{Kind: messaging.KindUser, Authority: "local", ID: "chris"}, ParentID: "p", Body: "interrupt", TargetSessionID: "s1", Actor: "msg://user/local/chris", Interrupt: true, Pending: true})
		if err != nil {
			t.Fatal(err)
		}
		snapshot := read(h, r.ReplyID)
		if err := h.st.PromoteRoutingReply(ctx, r.ReplyID); err != nil { // its caller was just told 202
			t.Fatal(err)
		}
		h.d.resolveStalePending(snapshot)
		if got := h.state(r.ReplyID); got.State != store.RoutingReplyQueued {
			t.Fatalf("the sweep settled a reply its caller was told was accepted: %+v", got)
		}
	})
	t.Run("an unchanged stale row is still resolved", func(t *testing.T) {
		h := newReplyHarness(t)
		h.rt.setAlive("s1", true, agentsessions.LiveStateProcessing)
		r := h.queued("s1", "x", false)
		if ok, _ := h.st.ClaimRoutingReply(ctx, r.ReplyID); !ok {
			t.Fatal("claim")
		}
		h.d.resolveStaleDelivering(read(h, r.ReplyID))
		if got := h.state(r.ReplyID); got.State != store.RoutingReplyUndeliverable || got.Reason != ReplyReasonRestart {
			t.Fatalf("a genuinely stale row was left alone: %+v", got)
		}
	})
}
