package msgstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/eventstore"
	"github.com/hollis-labs/tether/next/internal/store/msgstore"
	"github.com/hollis-labs/tether/next/internal/store/storetest"
)

func TestRoutingReplies(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms, clock := newStore(s)
		parent := send(t, ms, clock, op, alice)
		in := msgstore.NewRoutingReply{From: addr(t, op), ParentID: parent.ID, Body: "hello", TargetSessionID: "ses-1", Actor: "a", IdempotencyKey: "k1", Interrupt: true}
		r1, created, err := ms.CreateRoutingReply(ctx, in)
		if err != nil || !created || r1.State != msgstore.RoutingReplyQueued || !r1.Interrupt {
			t.Fatalf("create %+v %v %v", r1, created, err)
		}
		if again, created, _ := ms.CreateRoutingReply(ctx, in); created || again.ReplyID != r1.ReplyID {
			t.Fatalf("repeat created=%v %s", created, again.ReplyID)
		}
		conflict := in
		conflict.Body = "different"
		if _, _, err := ms.CreateRoutingReply(ctx, conflict); !errors.Is(err, msgstore.ErrRoutingReplyIdempotencyConflict) {
			t.Fatalf("conflict: %v", err)
		}
		if body, _ := ms.RoutingReplyBody(ctx, r1.ReplyID); body != "hello" {
			t.Fatalf("body %q", body)
		}
		if _, err := ms.Consume(ctx, r1.ReplyID, msgstore.RoutingReplyAddress); !errors.Is(err, msgstore.ErrRoutingReplyNotMailbox) {
			t.Fatalf("reply is a mailbox item: %v", err)
		}
		from, meta, _ := ms.RoutingReplySender(ctx, r1.ReplyID)
		if from.URN() != op || meta["routing_reply"] != "1" || meta["parent_id"] != parent.ID {
			t.Fatalf("sender %v %v", from, meta)
		}
		pending, _, _ := ms.CreateRoutingReply(ctx, msgstore.NewRoutingReply{From: addr(t, op), ParentID: parent.ID, Body: "later", TargetSessionID: "ses-1", Pending: true})
		if q, _ := ms.QueuedRoutingReplies(ctx, "ses-1", 0); len(q) != 1 {
			t.Fatalf("queued %d", len(q))
		}
		if err := ms.PromoteRoutingReply(ctx, pending.ReplyID); err != nil {
			t.Fatal(err)
		}
		if err := ms.PromoteRoutingReply(ctx, pending.ReplyID); !errors.Is(err, msgstore.ErrRoutingReplyState) {
			t.Fatalf("promote twice: %v", err)
		}
		q, _ := ms.QueuedRoutingReplies(ctx, "ses-1", 0)
		if len(q) != 2 || q[0].ReplyID != r1.ReplyID { // interrupts first
			t.Fatalf("queue order %+v", q)
		}
		if sessions, _ := ms.SessionsWithQueuedRoutingReplies(ctx); len(sessions) != 1 || sessions[0] != "ses-1" {
			t.Fatalf("sessions %v", sessions)
		}
		if ok, _ := ms.ClaimRoutingReply(ctx, r1.ReplyID); !ok {
			t.Fatal("claim failed")
		}
		claimed, _ := ms.RoutingReply(ctx, r1.ReplyID)
		if claimed.State != msgstore.RoutingReplyDelivering || claimed.Attempts != 1 {
			t.Fatalf("claimed %+v", claimed)
		}
		clock.Advance(time.Second)
		stale := claimed
		if err := ms.RequeueRoutingReply(ctx, r1.ReplyID, msgstore.RoutingReplyRequeue{Retarget: "ses-2", Reason: "busy", Detail: strings.Repeat("x ", 300),
			RefundAttempt: true, NotBefore: t0.Add(time.Minute), IfUnchanged: &claimed}); err != nil {
			t.Fatal(err)
		}
		requeued, _ := ms.RoutingReply(ctx, r1.ReplyID)
		if requeued.TargetSessionID != "ses-2" || requeued.Attempts != 0 || requeued.NextAttemptAt == nil || len(requeued.Detail) > msgstore.RoutingReplyDetailMax {
			t.Fatalf("requeued %+v", requeued)
		}
		if err := ms.SettleRoutingReply(ctx, r1.ReplyID, msgstore.RoutingReplySettlement{State: msgstore.RoutingReplyDelivered, IfUnchanged: &stale}); !errors.Is(err, msgstore.ErrRoutingReplyState) {
			t.Fatalf("settle with a stale snapshot: %v", err)
		}
		if err := ms.SettleRoutingReply(ctx, r1.ReplyID, msgstore.RoutingReplySettlement{State: msgstore.RoutingReplyQueued}); err == nil {
			t.Fatal("non-terminal settlement accepted")
		}
		if err := ms.SettleRoutingReply(ctx, r1.ReplyID, msgstore.RoutingReplySettlement{State: msgstore.RoutingReplyDelivered, DeliveredTo: "ses-2"}); err != nil {
			t.Fatal(err)
		}
		if got, _ := ms.Get(ctx, r1.ReplyID); got.ConsumedAt == nil {
			t.Fatal("delivered reply message not consumed")
		}
		if open, _ := ms.SessionHasOpenRoutingReplies(ctx, "ses-2"); open {
			t.Fatal("settled reply still open")
		}
		p2, _, _ := ms.CreateRoutingReply(ctx, msgstore.NewRoutingReply{From: addr(t, op), ParentID: parent.ID, Body: "drop", TargetSessionID: "ses-1", Pending: true})
		if err := ms.DiscardPendingRoutingReply(ctx, p2.ReplyID); err != nil {
			t.Fatal(err)
		}
		if _, err := ms.Get(ctx, p2.ReplyID); !errors.Is(err, messaging.ErrNotFound) {
			t.Fatalf("discarded message survived: %v", err)
		}
		if all, _ := ms.RoutingRepliesForParent(ctx, parent.ID); len(all) != 2 {
			t.Fatalf("for parent %d", len(all))
		}
		if _, err := ms.RoutingReply(ctx, "nope"); !errors.Is(err, msgstore.ErrRoutingReplyNotFound) {
			t.Fatalf("missing reply: %v", err)
		}
	})
}

func stage(t *testing.T, ms *msgstore.Store, session, turn, kind, body string, meta map[string]string) messaging.Envelope {
	t.Helper()
	m := map[string]string{"turn_id": turn, "kind": kind}
	for k, v := range meta {
		m[k] = v
	}
	env, err := ms.StageTurnOutput(context.Background(), messaging.Envelope{From: messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: session},
		Payload: json.RawMessage(body), ContentType: "application/json", Metadata: m})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestStagedTurnOutputs(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms, clock := newStore(s)
		a := stage(t, ms, "ses-1", "turn-1", "final", `{"text":"a"}`, nil)
		if again := stage(t, ms, "ses-1", "turn-1", "final", `{"text":"a"}`, nil); again.ID != a.ID {
			t.Fatal("same output got a new id")
		}
		alt := stage(t, ms, "ses-1", "turn-1", "final", `{"text":"changed"}`, nil)
		if alt.ID == a.ID || string(alt.Payload) != `{"text":"changed"}` {
			t.Fatalf("alternate body %+v", alt)
		}
		oid := strings.Repeat("ab", 32)
		stage(t, ms, "ses-1", "turn-2", "final", `{"text":"x"}`, map[string]string{"output_id": oid})
		if _, err := ms.StageTurnOutput(ctx, messaging.Envelope{From: messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "ses-1"},
			Payload: json.RawMessage(`{"text":"y"}`), Metadata: map[string]string{"output_id": oid}}); err == nil {
			t.Fatal("a different body under an output id was accepted")
		}
		if _, err := ms.StageTurnOutput(ctx, messaging.Envelope{From: addr(t, alice)}); err == nil {
			t.Fatal("a non-session sender was accepted")
		}
		if got, err := ms.StagedTurnOutput(ctx, a.ID); err != nil || got.To != msgstore.TurnOutputAddress || got.ThreadID != "ses-1" {
			t.Fatalf("staged %+v %v", got, err)
		}
		if _, err := ms.Get(ctx, a.ID); !errors.Is(err, messaging.ErrNotFound) {
			t.Fatal("a staged output is visible as a message")
		}
		page, err := ms.PendingTurnOutputs(ctx, msgstore.PendingTurnOutputCursor{}, 2)
		if err != nil || len(page) != 2 || page[0].SessionID != "ses-1" || len(page[0].CreatedAt) != len("2006-01-02T15:04:05.000000000Z") {
			t.Fatalf("pending page %+v %v", page, err)
		}
		rest, _ := ms.PendingTurnOutputs(ctx, msgstore.PendingTurnOutputCursor{CreatedAt: page[1].CreatedAt, MessageID: page[1].MessageID}, 10)
		if len(rest) != 1 {
			t.Fatalf("pending rest %+v", rest)
		}
		clock.Advance(msgstore.RoutingStageRetention + time.Hour)
		if old, _ := ms.PendingTurnOutputs(ctx, msgstore.PendingTurnOutputCursor{}, 10); len(old) != 0 {
			t.Fatalf("expired outputs still pending: %+v", old)
		}
	})
}

func TestChannelsAndAttach(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms, clock := newStore(s)
		es := eventstore.New(s)
		es.Now = clock.Now
		chURN := msgstore.ChannelURN("news")
		for i := 0; i < 3; i++ {
			if _, err := ms.Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, Channel: "news", From: addr(t, alice), To: addr(t, chURN),
				Payload: json.RawMessage(`{"n":1}`)}, true); err != nil {
				t.Fatal(err)
			}
			clock.Advance(time.Second)
		}
		names, _ := ms.ListChannelNames(ctx)
		hw, _ := ms.ChannelHighWater(ctx, "news")
		all, _ := ms.ReadChannel(ctx, "news", 0, 10)
		latest, _ := ms.ReadLatestChannel(ctx, "news", 2)
		if len(names) != 1 || len(all) != 3 || hw != all[2].Seq || len(latest) != 2 || latest[0].Seq != all[1].Seq {
			t.Fatalf("channel reads: names %v hw %d all %d latest %+v", names, hw, len(all), latest)
		}
		staged := stage(t, ms, "ses-1", "turn-1", "final", `{"text":"routed"}`, nil)
		attach := msgstore.ChannelAttach{MessageID: staged.ID, SessionID: "ses-1", Channel: "news", Kind: "final",
			SetMetadata: map[string]string{"launch_id": "L1", "launch_display_name": "seat"},
			Audit:       eventstore.Row{Scope: "session", SessionID: "ses-1", Kind: "session.turn.routed", PayloadJSON: `{"channel":"news"}`}}
		if _, err := ms.AttachStagedToChannel(ctx, msgstore.ChannelAttach{MessageID: staged.ID, SessionID: "ses-1", Channel: "news", Kind: "other"}, es); !errors.Is(err, msgstore.ErrAttachInvalid) {
			t.Fatalf("wrong kind: %v", err)
		}
		got, err := ms.AttachStagedToChannel(ctx, attach, es)
		if err != nil || got.To.URN() != chURN || got.Metadata["launch_id"] != "L1" {
			t.Fatalf("attach %+v %v", got, err)
		}
		if again, err := ms.AttachStagedToChannel(ctx, attach, es); err != nil || again.ID != staged.ID {
			t.Fatalf("repeat attach %v", err)
		}
		if all, _ = ms.ReadChannel(ctx, "news", 0, 10); len(all) != 4 || all[3].Envelope.ID != staged.ID {
			t.Fatalf("attached publication missing: %d", len(all))
		}
		pending, _ := es.Pending(ctx, 0, 0)
		if len(pending) != 1 || pending[0].Kind != "session.turn.routed" {
			t.Fatalf("outbox %+v", pending)
		}
		old := stage(t, ms, "ses-1", "turn-9", "final", `{"text":"old"}`, nil)
		clock.Advance(msgstore.RoutingStageRetention + time.Hour)
		if _, err := ms.AttachStagedToChannel(ctx, msgstore.ChannelAttach{MessageID: old.ID, SessionID: "ses-1", Channel: "news", Kind: "final", Audit: attach.Audit}, es); !errors.Is(err, messaging.ErrNotFound) {
			t.Fatalf("expired staged output: %v", err)
		}
	})
}

func TestA2ATasks(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms, clock := newStore(s)
		task := msgstore.A2ATask{BindingID: "b", TaskID: "t1", Owner: "o", ContextID: "c", State: "TASK_STATE_WORKING", TaskJSON: []byte(`{}`)}
		if v, err := ms.CreateA2ATask(ctx, task); err != nil || v != 1 {
			t.Fatalf("create %d %v", v, err)
		}
		if _, err := ms.CreateA2ATask(ctx, task); !errors.Is(err, msgstore.ErrA2ATaskExists) {
			t.Fatalf("duplicate: %v", err)
		}
		clock.Advance(time.Second)
		task.State = "TASK_STATE_COMPLETED"
		if v, err := ms.UpdateA2ATask(ctx, task, 1); err != nil || v != 2 {
			t.Fatalf("update %d %v", v, err)
		}
		if _, err := ms.UpdateA2ATask(ctx, task, 1); !errors.Is(err, msgstore.ErrA2ATaskConflict) {
			t.Fatalf("stale version: %v", err)
		}
		other := task
		other.Owner = "intruder"
		if _, err := ms.UpdateA2ATask(ctx, other, 0); !errors.Is(err, msgstore.ErrA2ATaskNotFound) {
			t.Fatalf("other owner: %v", err)
		}
		got, err := ms.GetA2ATask(ctx, "b", "t1")
		if err != nil || got.Version != 2 || got.State != "TASK_STATE_COMPLETED" {
			t.Fatalf("get %+v %v", got, err)
		}
		if l, _ := ms.ListA2ATasks(ctx, "b", "o", "c", "TASK_STATE_COMPLETED"); len(l) != 1 {
			t.Fatalf("list %+v", l)
		}
		if l, _ := ms.ListA2ATasksInStates(ctx, "TASK_STATE_WORKING"); len(l) != 0 {
			t.Fatalf("in states %+v", l)
		}
	})
}

// race runs fn on n goroutines at once and returns their results.
func race[T any](n int, fn func() (T, error)) ([]T, []error) {
	out, errs := make([]T, n), make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out[i], errs[i] = fn()
		}()
	}
	close(start)
	wg.Wait()
	return out, errs
}

// TestConcurrentCheckThenAct covers the two check-then-act paths under concurrent callers (they
// overlap on Postgres): a same-key routing reply create returns the one stored reply to every
// caller, and concurrent attaches of one staged output all return the publication, with one audit
// event.
func TestConcurrentCheckThenAct(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms, clock := newStore(s)
		es := eventstore.New(s)
		es.Now = clock.Now
		// Warm the pool, so the racers do not stagger on opening connections (one would otherwise
		// finish before the next has a connection, and nothing would overlap).
		warm := func() {
			race(6, func() (struct{}, error) {
				return struct{}{}, s.C().WriteTx(ctx, func(context.Context, store.Tx) error { time.Sleep(50 * time.Millisecond); return nil })
			})
		}
		warm()
		parent := send(t, ms, clock, op, alice)
		in := msgstore.NewRoutingReply{From: addr(t, op), ParentID: parent.ID, Body: "hello", TargetSessionID: "ses-1", Actor: "a", IdempotencyKey: "k-race"}
		type created struct {
			id  string
			new bool
		}
		replies, errs := race(6, func() (created, error) {
			r, isNew, err := ms.CreateRoutingReply(ctx, in)
			return created{r.ReplyID, isNew}, err
		})
		news := 0
		for i, r := range replies {
			if errs[i] != nil {
				t.Fatalf("concurrent create: %v", errs[i])
			}
			if r.id != replies[0].id {
				t.Fatalf("replies %+v", replies)
			}
			if r.new {
				news++
			}
		}
		if news != 1 {
			t.Fatalf("%d creates stored a reply, want 1", news)
		}

		// A JSON \u0000 escape is plain text: staged, and attached, on both dialects.
		staged := stage(t, ms, "ses-1", "turn-1", "final", `{"text":"a\u0000b"}`, nil)
		attach := msgstore.ChannelAttach{MessageID: staged.ID, SessionID: "ses-1", Channel: "news", Kind: "final",
			Audit: eventstore.Row{Scope: "session", SessionID: "ses-1", Kind: "session.turn.routed", PayloadJSON: `{"channel":"news"}`}}
		warm()
		envs, errs := race(6, func() (messaging.Envelope, error) { return ms.AttachStagedToChannel(ctx, attach, es) })
		for i, env := range envs {
			if errs[i] != nil || env.ID != staged.ID {
				t.Fatalf("concurrent attach: %v %+v", errs[i], env)
			}
		}
		if pending, _ := es.Pending(ctx, 0, 0); len(pending) != 1 {
			t.Fatalf("audit events %d, want 1", len(pending))
		}
	})
}

// TestStagedRawBytes: a raw NUL byte or invalid UTF-8 in a turn output is stored as before on
// SQLite (parity) and refused, typed, on Postgres, whose text cannot hold it.
func TestStagedRawBytes(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms, _ := newStore(s)
		for i, body := range []string{"a\x00b", "a\xffb"} {
			got, err := ms.StageTurnOutput(ctx, messaging.Envelope{From: messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "ses-raw"},
				Payload: json.RawMessage(body), ContentType: "text/plain", Metadata: map[string]string{"turn_id": fmt.Sprint("turn-raw-", i), "kind": "final"}})
			if s.C().Dialect().Name() == gostore.DriverPostgres {
				if !errors.Is(err, msgstore.ErrStagedBodyUnstorable) {
					t.Fatalf("postgres, body %q: %v", body, err)
				}
				continue
			}
			if err != nil || string(got.Payload) != body {
				t.Fatalf("sqlite, body %q: stored %q %v", body, got.Payload, err)
			}
			if back, err := ms.StagedTurnOutput(ctx, got.ID); err != nil || string(back.Payload) != body {
				t.Fatalf("sqlite, body %q read back %q %v", body, back.Payload, err)
			}
		}
	})
}
