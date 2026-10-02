package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/delivery"
	"github.com/hollis-labs/tether/internal/store"
)

func newReply(body string) store.NewRoutingReply {
	return store.NewRoutingReply{
		From:     messaging.Address{Kind: messaging.KindUser, Authority: "local", ID: "chris"},
		ParentID: "parent-1", Body: body, TargetSessionID: "sess-a", LogicalAgentID: "agent-a", Actor: "msg://user/local/chris",
	}
}

func TestRoutingReplyIsQueuedOnceWithoutAnyDeliveryObligation(t *testing.T) {
	db := openRetentionDB(t)
	ctx := context.Background()
	reply, created, err := db.CreateRoutingReply(ctx, newReply("use the second option"))
	if err != nil || !created {
		t.Fatalf("create: %v created=%v", err, created)
	}
	if reply.State != store.RoutingReplyQueued || reply.TargetSessionID != "sess-a" || reply.OriginalSessionID != "sess-a" || reply.Attempts != 0 {
		t.Fatalf("reply = %+v", reply)
	}
	if body, err := db.RoutingReplyBody(ctx, reply.ReplyID); err != nil || body != "use the second option" {
		t.Fatalf("body %q %v", body, err)
	}

	// Thread readers see it; the target session's inbox does not, so the reply
	// cannot become a second copy of the turn the dispatcher injects.
	env, err := db.MessagingStore().Get(ctx, reply.ReplyID)
	if err != nil || env.InReplyTo != "parent-1" || env.ThreadID != "parent-1" || env.To != store.RoutingReplyAddress {
		t.Fatalf("Get: %+v %v", env, err)
	}
	session := messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "sess-a"}
	if rows, err := db.MessagingStore().Inbox(ctx, session, messaging.Filter{}); err != nil || len(rows) != 0 {
		t.Fatalf("session inbox: %+v %v", rows, err)
	}
	if _, err := store.ImportLegacyMessagesIntoDelivery(ctx, db.DB(), db.DeliveryStore(), store.ImportHoldAmbiguousDelivered); err != nil {
		t.Fatal(err)
	}
	if ready, err := db.DeliveryStore().ListDeliveries(ctx, delivery.Filter{}); err != nil || len(ready) != 0 {
		t.Fatalf("a delivery obligation was created: %+v %v", ready, err)
	}
}

func TestRoutingReplyIdempotencyKey(t *testing.T) {
	db := openRetentionDB(t)
	ctx := context.Background()
	in := newReply("once")
	in.IdempotencyKey = "k1"
	first, created, err := db.CreateRoutingReply(ctx, in)
	if err != nil || !created {
		t.Fatal(err, created)
	}
	again, created, err := db.CreateRoutingReply(ctx, in)
	if err != nil || created || again.ReplyID != first.ReplyID {
		t.Fatalf("retry: %+v created=%v %v", again, created, err)
	}
	in.Body = "twice"
	if _, _, err := db.CreateRoutingReply(ctx, in); !errors.Is(err, store.ErrRoutingReplyIdempotencyConflict) {
		t.Fatalf("different body under the same key: %v", err)
	}
	other := newReply("once")
	other.IdempotencyKey = "k1"
	other.Actor = "msg://user/local/someone-else"
	if second, created, err := db.CreateRoutingReply(ctx, other); err != nil || !created || second.ReplyID == first.ReplyID {
		t.Fatalf("keys are per actor: %+v %v %v", second, created, err)
	}
	in.IdempotencyKey = ""
	in.Body = "once"
	if a, _, err := db.CreateRoutingReply(ctx, in); err != nil || a.ReplyID == first.ReplyID {
		t.Fatalf("no key means no dedupe: %v", err)
	}
}

func TestRoutingReplyClaimIsExclusiveAndQueueIsFIFO(t *testing.T) {
	db := openRetentionDB(t)
	ctx := context.Background()
	var ids []string
	for _, body := range []string{"one", "two", "three"} {
		r, _, err := db.CreateRoutingReply(ctx, newReply(body))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ReplyID)
	}
	queued, err := db.QueuedRoutingReplies(ctx, "sess-a", time.Now(), 10)
	if err != nil || len(queued) != 3 {
		t.Fatalf("queued: %+v %v", queued, err)
	}
	for i, r := range queued {
		if r.ReplyID != ids[i] {
			t.Fatalf("order[%d] = %s, want %s", i, r.ReplyID, ids[i])
		}
	}
	if other, err := db.QueuedRoutingReplies(ctx, "sess-b", time.Now(), 10); err != nil || len(other) != 0 {
		t.Fatalf("another session's queue: %+v %v", other, err)
	}
	if ok, err := db.ClaimRoutingReply(ctx, ids[0]); err != nil || !ok {
		t.Fatalf("first claim: %v %v", ok, err)
	}
	if ok, err := db.ClaimRoutingReply(ctx, ids[0]); err != nil || ok {
		t.Fatalf("second claim must lose: %v %v", ok, err)
	}
	claimed, _ := db.RoutingReply(ctx, ids[0])
	if claimed.State != store.RoutingReplyDelivering || claimed.Attempts != 1 {
		t.Fatalf("claimed = %+v", claimed)
	}
	if left, _ := db.QueuedRoutingReplies(ctx, "sess-a", time.Now(), 10); len(left) != 2 {
		t.Fatalf("a claimed reply is still queued: %+v", left)
	}
}

func TestRoutingReplyRequeueRetargetsAndHonoursNotBefore(t *testing.T) {
	db := openRetentionDB(t)
	ctx := context.Background()
	r, _, _ := db.CreateRoutingReply(ctx, newReply("again"))
	if ok, _ := db.ClaimRoutingReply(ctx, r.ReplyID); !ok {
		t.Fatal("claim")
	}
	later := time.Now().Add(time.Hour)
	if err := db.RequeueRoutingReply(ctx, r.ReplyID, "sess-b", "handed_off", later); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.QueuedRoutingReplies(ctx, "sess-b", time.Now(), 10); len(got) != 0 {
		t.Fatalf("returned before its retry time: %+v", got)
	}
	got, err := db.QueuedRoutingReplies(ctx, "sess-b", later.Add(time.Second), 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("after retry time: %+v %v", got, err)
	}
	if got[0].OriginalSessionID != "sess-a" || got[0].TargetSessionID != "sess-b" || got[0].Reason != "handed_off" || got[0].Attempts != 1 {
		t.Fatalf("requeued = %+v", got[0])
	}
}

func TestRoutingReplySettleIsTerminalAndMarksTheMessageForRetention(t *testing.T) {
	db := openRetentionDB(t)
	ctx := context.Background()
	delivered, _, _ := db.CreateRoutingReply(ctx, newReply("ok"))
	dead, _, _ := db.CreateRoutingReply(ctx, newReply("lost"))
	if ok, _ := db.ClaimRoutingReply(ctx, delivered.ReplyID); !ok {
		t.Fatal("claim")
	}
	if err := db.SettleRoutingReply(ctx, delivered.ReplyID, store.RoutingReplyDelivered, "", "sess-a"); err != nil {
		t.Fatal(err)
	}
	if err := db.SettleRoutingReply(ctx, dead.ReplyID, store.RoutingReplyUndeliverable, "session_ended_no_binding", ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{delivered.ReplyID, dead.ReplyID} {
		if err := db.SettleRoutingReply(ctx, id, store.RoutingReplyDelivered, "", "sess-a"); !errors.Is(err, store.ErrRoutingReplyState) {
			t.Fatalf("settling twice: %v", err)
		}
		if err := db.RequeueRoutingReply(ctx, id, "", "x", time.Time{}); !errors.Is(err, store.ErrRoutingReplyState) {
			t.Fatalf("requeue of a settled reply: %v", err)
		}
		if ok, _ := db.ClaimRoutingReply(ctx, id); ok {
			t.Fatal("claimed a settled reply")
		}
	}
	got, _ := db.RoutingReply(ctx, dead.ReplyID)
	if got.State != store.RoutingReplyUndeliverable || got.Reason != "session_ended_no_binding" || got.SettledAt == nil {
		t.Fatalf("undeliverable = %+v", got)
	}
	if err := db.SettleRoutingReply(ctx, "nope", store.RoutingReplyQueued, "", ""); err == nil {
		t.Fatal("queued is not a terminal state")
	}
	cands, err := db.ListRetentionCandidates(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	eligible := map[string]bool{}
	for _, c := range cands {
		eligible[c.MessageID] = c.Eligible
	}
	if !eligible[delivered.ReplyID] || !eligible[dead.ReplyID] {
		t.Fatalf("finished replies must be purge-eligible: %+v", eligible)
	}
}

func TestRoutingReplyUnsettledBodyIsNeverPurgeEligible(t *testing.T) {
	db := openRetentionDB(t)
	ctx := context.Background()
	r, _, _ := db.CreateRoutingReply(ctx, newReply("still waiting"))
	cands, err := db.ListRetentionCandidates(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		if c.MessageID == r.ReplyID && c.Eligible {
			t.Fatal("a queued reply's only copy must not be purgeable")
		}
	}
}

func TestRoutingReplyBodyPurgedIsReportedNotInvented(t *testing.T) {
	db := openRetentionDB(t)
	ctx := context.Background()
	r, _, _ := db.CreateRoutingReply(ctx, newReply("gone"))
	if err := db.SettleRoutingReply(ctx, r.ReplyID, store.RoutingReplyUndeliverable, "x", ""); err != nil {
		t.Fatal(err)
	}
	if purged, err := db.PurgeMessageBody(ctx, r.ReplyID, "msg://user/local/chris"); err != nil || !purged {
		t.Fatalf("purge: %v %v", purged, err)
	}
	if _, err := db.RoutingReplyBody(ctx, r.ReplyID); !errors.Is(err, store.ErrRoutingReplyBodyPurged) {
		t.Fatalf("body after purge: %v", err)
	}
	if _, err := db.RoutingReplyBody(ctx, "missing"); !errors.Is(err, store.ErrRoutingReplyNotFound) {
		t.Fatalf("unknown reply: %v", err)
	}
}

func TestRoutingRepliesInStateAndForParent(t *testing.T) {
	db := openRetentionDB(t)
	ctx := context.Background()
	a, _, _ := db.CreateRoutingReply(ctx, newReply("a"))
	b, _, _ := db.CreateRoutingReply(ctx, newReply("b"))
	if ok, _ := db.ClaimRoutingReply(ctx, b.ReplyID); !ok {
		t.Fatal("claim")
	}
	if got, _ := db.RoutingRepliesInState(ctx, store.RoutingReplyDelivering, 10); len(got) != 1 || got[0].ReplyID != b.ReplyID {
		t.Fatalf("delivering: %+v", got)
	}
	if got, _ := db.RoutingRepliesForParent(ctx, "parent-1"); len(got) != 2 || got[0].ReplyID != a.ReplyID {
		t.Fatalf("for parent: %+v", got)
	}
}
