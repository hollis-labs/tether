package store_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	queued, err := db.QueuedRoutingReplies(ctx, "sess-a", 10)
	if err != nil || len(queued) != 3 {
		t.Fatalf("queued: %+v %v", queued, err)
	}
	for i, r := range queued {
		if r.ReplyID != ids[i] {
			t.Fatalf("order[%d] = %s, want %s", i, r.ReplyID, ids[i])
		}
	}
	if other, err := db.QueuedRoutingReplies(ctx, "sess-b", 10); err != nil || len(other) != 0 {
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
	if left, _ := db.QueuedRoutingReplies(ctx, "sess-a", 10); len(left) != 2 {
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
	if err := db.RequeueRoutingReply(ctx, r.ReplyID, store.RoutingReplyRequeue{Retarget: "sess-b", Reason: "handed_off", NotBefore: later}); err != nil {
		t.Fatal(err)
	}
	// A backing-off reply is still the head of its session's queue: it carries its
	// retry time and the dispatcher waits, so a younger due reply cannot jump it.
	got, err := db.QueuedRoutingReplies(ctx, "sess-b", 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("queue: %+v %v", got, err)
	}
	if got[0].NextAttemptAt == nil || got[0].NextAttemptAt.Before(later.Add(-time.Second)) {
		t.Fatalf("retry time lost: %+v", got[0])
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
	if err := db.SettleRoutingReply(ctx, delivered.ReplyID, store.RoutingReplySettlement{State: store.RoutingReplyDelivered, DeliveredTo: "sess-a"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SettleRoutingReply(ctx, dead.ReplyID, store.RoutingReplySettlement{State: store.RoutingReplyUndeliverable, Reason: "session_ended_no_binding", Detail: "the actor has no binding"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{delivered.ReplyID, dead.ReplyID} {
		if err := db.SettleRoutingReply(ctx, id, store.RoutingReplySettlement{State: store.RoutingReplyDelivered, DeliveredTo: "sess-a"}); !errors.Is(err, store.ErrRoutingReplyState) {
			t.Fatalf("settling twice: %v", err)
		}
		if err := db.RequeueRoutingReply(ctx, id, store.RoutingReplyRequeue{Reason: "x"}); !errors.Is(err, store.ErrRoutingReplyState) {
			t.Fatalf("requeue of a settled reply: %v", err)
		}
		if ok, _ := db.ClaimRoutingReply(ctx, id); ok {
			t.Fatal("claimed a settled reply")
		}
	}
	// Only a delivered reply reads as consumed; an undeliverable one never was.
	if env, err := db.MessagingStore().Get(ctx, delivered.ReplyID); err != nil || env.ConsumedAt == nil {
		t.Fatalf("delivered reply not stamped consumed: %+v %v", env, err)
	}
	if env, err := db.MessagingStore().Get(ctx, dead.ReplyID); err != nil || env.ConsumedAt != nil {
		t.Fatalf("undeliverable reply stamped consumed: %+v %v", env, err)
	}
	got, _ := db.RoutingReply(ctx, dead.ReplyID)
	if got.State != store.RoutingReplyUndeliverable || got.Reason != "session_ended_no_binding" || got.Detail != "the actor has no binding" || got.SettledAt == nil {
		t.Fatalf("undeliverable = %+v", got)
	}
	if err := db.SettleRoutingReply(ctx, "nope", store.RoutingReplySettlement{State: store.RoutingReplyQueued}); err == nil {
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
	if err := db.SettleRoutingReply(ctx, r.ReplyID, store.RoutingReplySettlement{State: store.RoutingReplyUndeliverable, Reason: "x"}); err != nil {
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

func TestRoutingReplyInterruptingRepliesComeFirstThenArrivalOrder(t *testing.T) {
	db := openRetentionDB(t)
	ctx := context.Background()
	mk := func(body string, interrupt bool) string {
		in := newReply(body)
		in.Interrupt = interrupt
		r, _, err := db.CreateRoutingReply(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return r.ReplyID
	}
	a, b := mk("older", false), mk("newer", false)
	c, d := mk("interrupting one", true), mk("interrupting two", true)
	got, err := db.QueuedRoutingReplies(ctx, "sess-a", 10)
	if err != nil || len(got) != 4 {
		t.Fatalf("queue: %+v %v", got, err)
	}
	for i, want := range []string{c, d, a, b} {
		if got[i].ReplyID != want {
			t.Fatalf("position %d = %s, want %s (interrupting replies first, each group in arrival order)", i, got[i].ReplyID, want)
		}
	}
}

func TestRoutingReplyPendingIsReservedButNeverQueued(t *testing.T) {
	db := openRetentionDB(t)
	ctx := context.Background()
	in := newReply("interrupt me")
	in.Interrupt, in.Pending, in.IdempotencyKey = true, true, "k"
	pending, created, err := db.CreateRoutingReply(ctx, in)
	if err != nil || !created || pending.State != store.RoutingReplyPending {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	if got, _ := db.QueuedRoutingReplies(ctx, "sess-a", 10); len(got) != 0 {
		t.Fatalf("a pending reply is deliverable: %+v", got)
	}
	if sessions, _ := db.SessionsWithQueuedRoutingReplies(ctx); len(sessions) != 0 {
		t.Fatalf("sweep would drain a pending reply: %v", sessions)
	}
	// The reservation holds the idempotency key: a retry finds it, it does not create a twin.
	if again, found, err := db.PeekRoutingReply(ctx, in); err != nil || !found || again.ReplyID != pending.ReplyID {
		t.Fatalf("peek: %+v %v %v", again, found, err)
	}
	if ok, _ := db.ClaimRoutingReply(ctx, pending.ReplyID); ok {
		t.Fatal("claimed a pending reply")
	}
	if err := db.PromoteRoutingReply(ctx, pending.ReplyID); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.QueuedRoutingReplies(ctx, "sess-a", 10); len(got) != 1 || got[0].ReplyID != pending.ReplyID {
		t.Fatalf("promoted reply not queued: %+v", got)
	}
	if err := db.PromoteRoutingReply(ctx, pending.ReplyID); !errors.Is(err, store.ErrRoutingReplyState) {
		t.Fatalf("promoting twice: %v", err)
	}
	if err := db.DiscardPendingRoutingReply(ctx, pending.ReplyID); !errors.Is(err, store.ErrRoutingReplyState) {
		t.Fatalf("an accepted reply must never be discarded: %v", err)
	}
}

func TestRoutingReplyDiscardRemovesOnlyAPendingReplyAndItsBody(t *testing.T) {
	db := openRetentionDB(t)
	ctx := context.Background()
	in := newReply("refused")
	in.Pending, in.IdempotencyKey = true, "k"
	pending, _, _ := db.CreateRoutingReply(ctx, in)
	if err := db.DiscardPendingRoutingReply(ctx, pending.ReplyID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RoutingReply(ctx, pending.ReplyID); !errors.Is(err, store.ErrRoutingReplyNotFound) {
		t.Fatalf("reply survived: %v", err)
	}
	if _, err := db.MessagingStore().Get(ctx, pending.ReplyID); !errors.Is(err, messaging.ErrNotFound) {
		t.Fatalf("message survived: %v", err)
	}
	// The key is free again: the refused reply left nothing behind.
	if again, created, err := db.CreateRoutingReply(ctx, in); err != nil || !created || again.ReplyID == pending.ReplyID {
		t.Fatalf("recreate: %+v %v %v", again, created, err)
	}
	// Settling a pending reply (stale reservation found after a crash) is allowed.
	stale, _, _ := db.CreateRoutingReply(ctx, func() store.NewRoutingReply { x := newReply("stale"); x.Pending = true; return x }())
	if err := db.SettleRoutingReply(ctx, stale.ReplyID, store.RoutingReplySettlement{State: store.RoutingReplyUndeliverable, Reason: "interrupt_unconfirmed"}); err != nil {
		t.Fatal(err)
	}
}

func TestRoutingReplyIdempotencyKeyIsScopedToTheParent(t *testing.T) {
	db := openRetentionDB(t)
	ctx := context.Background()
	a := newReply("same body")
	a.IdempotencyKey = "k"
	b := a
	b.ParentID = "parent-2"
	first, _, err := db.CreateRoutingReply(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	second, created, err := db.CreateRoutingReply(ctx, b)
	if err != nil || !created || second.ReplyID == first.ReplyID {
		t.Fatalf("the same key on another parent must be another reply: %+v %v %v", second, created, err)
	}
}

func TestRoutingReplyRecordsTheActorAndBoundsDetail(t *testing.T) {
	db := openRetentionDB(t)
	ctx := context.Background()
	r, _, _ := db.CreateRoutingReply(ctx, newReply("who sent this"))
	if r.Actor != "msg://user/local/chris" {
		t.Fatalf("actor = %q", r.Actor)
	}
	// A subprocess error carries up to 2 KB of stderr across several lines.
	noisy := "runner: process exited 1\nstderr:\n" + strings.Repeat("secret token = abc \n", 100)
	if err := db.SettleRoutingReply(ctx, r.ReplyID, store.RoutingReplySettlement{State: store.RoutingReplyUndeliverable, Reason: "submit_failed", Detail: noisy}); err != nil {
		t.Fatal(err)
	}
	got, _ := db.RoutingReply(ctx, r.ReplyID)
	if len(got.Detail) > store.RoutingReplyDetailMax || strings.Contains(got.Detail, "\n") || !strings.HasPrefix(got.Detail, "runner: process exited 1 stderr:") {
		t.Fatalf("detail = %q (%d bytes)", got.Detail, len(got.Detail))
	}
	if d := store.BoundRoutingDetail(strings.Repeat("é", 400)); len(d) > store.RoutingReplyDetailMax || !utf8.ValidString(d) {
		t.Fatalf("detail cut mid-rune: %q", d)
	}
}

// The sweep runs every few seconds over a table whose settled rows are never
// deleted: it must read the by-state index, not the whole history.
func TestRoutingReplySweepQueriesUseTheStateIndex(t *testing.T) {
	db := openRetentionDB(t)
	for _, q := range []string{
		`SELECT DISTINCT target_session_id FROM routing_replies WHERE state='queued'`,
		`SELECT reply_id FROM routing_replies WHERE state='delivering' ORDER BY created_at, reply_id LIMIT 1000`,
		`SELECT reply_id FROM routing_replies WHERE state='pending' ORDER BY created_at, reply_id LIMIT 1000`,
	} {
		rows, err := db.DB().Query(`EXPLAIN QUERY PLAN ` + q)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(detail + "\n")
		}
		_ = rows.Close()
		if !strings.Contains(plan.String(), "idx_routing_replies_state") || strings.Contains(plan.String(), "SCAN routing_replies\n") {
			t.Fatalf("%s\nplan: %s", q, plan.String())
		}
	}
}

func TestMailboxVerbsOnAReplyAreRefusedAndTheRowIsUntouched(t *testing.T) {
	db := openRetentionDB(t)
	ctx := context.Background()
	r, _, _ := db.CreateRoutingReply(ctx, newReply("deliver me"))
	ms := db.MessagingStore()
	to := store.RoutingReplyAddress
	for name, call := range map[string]func() error{
		"cancel":    func() error { return ms.Cancel(ctx, r.ReplyID) },
		"consume":   func() error { return ms.Consume(ctx, r.ReplyID, to) },
		"mark read": func() error { return ms.MarkRead(ctx, r.ReplyID, to) },
		"archive":   func() error { return ms.Archive(ctx, r.ReplyID, to) },
		"unarchive": func() error { return ms.Unarchive(ctx, r.ReplyID, to) },
	} {
		if err := call(); !errors.Is(err, store.ErrRoutingReplyNotMailbox) {
			t.Errorf("%s: %v", name, err)
		}
	}
	var canceled, consumed, read, archived *string
	if err := db.DB().QueryRow(`SELECT canceled_at, consumed_at, read_at, archived_at FROM messages WHERE id=?`, r.ReplyID).
		Scan(&canceled, &consumed, &read, &archived); err != nil {
		t.Fatal(err)
	}
	if canceled != nil || consumed != nil || read != nil || archived != nil {
		t.Fatalf("a refused verb changed the row: %v %v %v %v", canceled, consumed, read, archived)
	}
}

// The repair sweep drains exactly the sessions that have a reply waiting: one
// entry per session however many replies it has, and nothing for a reply that is
// only reserved, or already finished.
func TestRoutingReplySessionsWithQueuedAreTheDistinctQueuedTargets(t *testing.T) {
	db := openRetentionDB(t)
	ctx := context.Background()
	add := func(session string, pending bool) store.RoutingReply {
		in := newReply("x")
		in.TargetSessionID, in.Pending = session, pending
		r, _, err := db.CreateRoutingReply(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	add("sess-a", false)
	add("sess-a", false)
	add("sess-b", false)
	add("sess-c", true) // reserved by an interrupting submission, not deliverable yet
	done := add("sess-d", false)
	if ok, err := db.ClaimRoutingReply(ctx, done.ReplyID); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := db.SettleRoutingReply(ctx, done.ReplyID, store.RoutingReplySettlement{State: store.RoutingReplyDelivered, DeliveredTo: "sess-d"}); err != nil {
		t.Fatal(err)
	}
	got, err := db.SessionsWithQueuedRoutingReplies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "sess-a" || got[1] != "sess-b" {
		t.Fatalf("sessions with a queued reply = %v, want [sess-a sess-b]", got)
	}
}

// A caller that acts on a snapshot (the stale-row sweep) must not overwrite a
// transition that happened after it read: the write applies only to the exact row
// version it saw, and an up-to-date snapshot still applies.
func TestRoutingReplyConditionalSettleAndRequeueRefuseARowThatMovedSinceItWasRead(t *testing.T) {
	db := openRetentionDB(t)
	ctx := context.Background()
	read := func(id string) store.RoutingReply {
		r, err := db.RoutingReply(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	in := newReply("x")
	in.Pending = true
	created, _, err := db.CreateRoutingReply(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	id := created.ReplyID

	// Promoted after the snapshot: settling from the pending snapshot is refused.
	pending := read(id)
	if err := db.PromoteRoutingReply(ctx, id); err != nil {
		t.Fatal(err)
	}
	err = db.SettleRoutingReply(ctx, id, store.RoutingReplySettlement{State: store.RoutingReplyUndeliverable, Reason: "r", IfUnchanged: &pending})
	if !errors.Is(err, store.ErrRoutingReplyState) || read(id).State != store.RoutingReplyQueued {
		t.Fatalf("settle from a stale pending snapshot: %v, state %q", err, read(id).State)
	}

	// Requeued and claimed again after the snapshot: same state, newer version.
	if ok, err := db.ClaimRoutingReply(ctx, id); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	delivering := read(id)
	if err := db.RequeueRoutingReply(ctx, id, store.RoutingReplyRequeue{Reason: "again"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ClaimRoutingReply(ctx, id); err != nil || !ok {
		t.Fatalf("claim again: %v %v", ok, err)
	}
	err = db.RequeueRoutingReply(ctx, id, store.RoutingReplyRequeue{Reason: "stale", IfUnchanged: &delivering})
	if !errors.Is(err, store.ErrRoutingReplyState) || read(id).State != store.RoutingReplyDelivering || read(id).Reason != "again" {
		t.Fatalf("requeue from a stale delivering snapshot: %v, row %+v", err, read(id))
	}
	err = db.SettleRoutingReply(ctx, id, store.RoutingReplySettlement{State: store.RoutingReplyUndeliverable, Reason: "stale", IfUnchanged: &delivering})
	if !errors.Is(err, store.ErrRoutingReplyState) || read(id).State != store.RoutingReplyDelivering {
		t.Fatalf("settle from a stale delivering snapshot: %v, row %+v", err, read(id))
	}

	// A current snapshot applies.
	current := read(id)
	if err := db.RequeueRoutingReply(ctx, id, store.RoutingReplyRequeue{Reason: "current", IfUnchanged: &current}); err != nil {
		t.Fatalf("requeue from a current snapshot: %v", err)
	}
	current = read(id)
	if err := db.SettleRoutingReply(ctx, id, store.RoutingReplySettlement{State: store.RoutingReplyUndeliverable, Reason: "current", IfUnchanged: &current}); err != nil {
		t.Fatalf("settle from a current snapshot: %v", err)
	}
	if got := read(id); got.State != store.RoutingReplyUndeliverable || got.Reason != "current" {
		t.Fatalf("row = %+v", got)
	}
}
