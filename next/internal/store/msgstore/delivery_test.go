package msgstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/libs/mesh/core/messaging/delivery"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/eventstore"
	"github.com/hollis-labs/tether/next/internal/store/msgstore"
	"github.com/hollis-labs/tether/next/internal/store/storetest"
)

// warmPool opens the class C pool's connections before a race, so racers do not stagger on
// connection setup and actually overlap on Postgres.
func warmPool(ctx context.Context, s *store.Stores) {
	race(6, func() (struct{}, error) {
		return struct{}{}, s.C().WriteTx(ctx, func(context.Context, store.Tx) error { time.Sleep(50 * time.Millisecond); return nil })
	})
}

func TestDeliveryRows(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms, clock := newStore(s)
		m := send(t, ms, clock, alice, bob)
		if err := ms.SetPendingReceiptLease(ctx, m.ID, "att-1", "tok-1"); err != nil {
			t.Fatal(err)
		}
		if a, l, err := ms.PendingReceiptLease(ctx, m.ID); err != nil || a != "att-1" || l != "tok-1" {
			t.Fatalf("pending lease %q %q %v", a, l, err)
		}
		if handled, err := ms.MessageHandledByRecipient(ctx, m.ID); err != nil || handled {
			t.Fatalf("handled before read: %v %v", handled, err)
		}
		if err := ms.MarkRead(ctx, m.ID, addr(t, bob)); err != nil {
			t.Fatal(err)
		}
		if handled, _ := ms.MessageHandledByRecipient(ctx, m.ID); !handled {
			t.Fatal("read message not handled")
		}
		if _, ok, err := ms.DeliveryIDForMessage(ctx, m.ID); err != nil || ok {
			t.Fatalf("untracked message: %v %v", ok, err)
		}
		if _, _, err := ms.DeliveryIDForMessage(ctx, "nope"); !errors.Is(err, messaging.ErrNotFound) {
			t.Fatalf("unknown message: %v", err)
		}
		other := send(t, ms, clock, alice, bob)
		cands, err := ms.LegacyImportCandidates(ctx)
		if err != nil || len(cands) != 2 || cands[0].ID != m.ID || cands[0].DeliveredAt.Valid {
			t.Fatalf("candidates %+v %v", cands, err)
		}
		if ok, err := ms.SetLegacyDeliveryID(ctx, m.ID, "dlv-1"); err != nil || !ok {
			t.Fatalf("set delivery id %v %v", ok, err)
		}
		if ok, _ := ms.SetLegacyDeliveryID(ctx, m.ID, "dlv-2"); ok {
			t.Fatal("a second import replaced the delivery id")
		}
		if id, ok, _ := ms.DeliveryIDForMessage(ctx, m.ID); !ok || id != "dlv-1" {
			t.Fatalf("delivery id %q %v", id, ok)
		}
		if id, _ := ms.MailboxDeliveryID(ctx, m.ID); id != "dlv-1" {
			t.Fatalf("mailbox delivery id %q", id)
		}
		if cands, _ = ms.LegacyImportCandidates(ctx); len(cands) != 1 || cands[0].ID != other.ID {
			t.Fatalf("candidates after import %+v", cands)
		}
		if _, _, err := ms.ClaimMessageDelivery(ctx, other.ID, addr(t, alice), "h", time.Minute); !errors.Is(err, msgstore.ErrWrongRecipient) {
			t.Fatalf("wrong recipient claim: %v", err)
		}
		if _, _, err := ms.ClaimMessageDelivery(ctx, other.ID, addr(t, bob), "h", time.Minute); !errors.Is(err, msgstore.ErrNoDeliveryTracking) {
			t.Fatalf("untracked claim: %v", err)
		}
	})
}

func TestSettleReadUndelivered(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms, clock := newStore(s)
		a := send(t, ms, clock, alice, bob)
		b := send(t, ms, clock, alice, bob)
		send(t, ms, clock, alice, bob) // unread: not settled
		for _, id := range []string{a.ID, b.ID} {
			if err := ms.MarkRead(ctx, id, addr(t, bob)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := ms.SetLegacyDeliveryID(ctx, b.ID, "dlv-b"); err != nil {
			t.Fatal(err)
		}
		got, err := ms.SettleReadUndelivered(ctx, addr(t, bob), messaging.Filter{})
		if err != nil || len(got) != 2 || got[0] != (msgstore.SettledRead{MessageID: a.ID}) || got[1] != (msgstore.SettledRead{MessageID: b.ID, DeliveryID: "dlv-b"}) {
			t.Fatalf("settled %+v %v", got, err)
		}
		if got, _ = ms.SettleReadUndelivered(ctx, addr(t, bob), messaging.Filter{}); len(got) != 0 {
			t.Fatalf("settled twice %+v", got)
		}

		// Concurrent pulls: every read message is reported settled exactly once.
		const total = 40
		for range total {
			m := send(t, ms, clock, alice, alice)
			if err := ms.MarkRead(ctx, m.ID, addr(t, alice)); err != nil {
				t.Fatal(err)
			}
		}
		warmPool(ctx, s)
		pulls, errs := race(6, func() ([]msgstore.SettledRead, error) {
			return ms.SettleReadUndelivered(ctx, addr(t, alice), messaging.Filter{})
		})
		seen := map[string]int{}
		for i, p := range pulls {
			if errs[i] != nil {
				t.Fatal(errs[i])
			}
			for _, r := range p {
				seen[r.MessageID]++
			}
		}
		if len(seen) != total {
			t.Fatalf("settled %d distinct, want %d", len(seen), total)
		}
		for id, n := range seen {
			if n != 1 {
				t.Fatalf("%s settled %d times", id, n)
			}
		}
	})
}

func lifecycle(kind string) eventstore.Row {
	return eventstore.Row{Scope: "daemon", Kind: kind, PayloadJSON: `{}`}
}

func TestCommitMessageEvents(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms, _ := newStore(s)
		es := eventstore.New(s)
		cp, err := ms.MessageEventCheckpoints(ctx)
		if err != nil || cp != (msgstore.MessageEventCheckpoints{Delivery: -1, Attention: 0, Retry: 0}) {
			t.Fatalf("seed checkpoints %+v %v", cp, err)
		}
		if n, err := ms.CommitMessageEvents(ctx, msgstore.DrainBatch{From: cp}, es); err != nil || n != 0 {
			t.Fatalf("empty batch %d %v", n, err)
		}
		batch := msgstore.DrainBatch{From: cp,
			Delivery: []msgstore.DrainRecord{
				{Position: 5, At: t0.Add(2 * time.Second), Events: []eventstore.Row{lifecycle("message.sent"), lifecycle("message.queued")}},
				{Position: 6, At: t0.Add(4 * time.Second), Deferred: true},
			},
			Attention: []msgstore.DrainRecord{
				{Position: 1, At: t0.Add(time.Second), Events: []eventstore.Row{lifecycle("message.read")}},
				{Position: 2, At: t0.Add(4 * time.Second)}, // a tie goes to delivery first
			},
		}
		if n, err := ms.CommitMessageEvents(ctx, batch, es); err != nil || n != 4 {
			t.Fatalf("commit %d %v", n, err)
		}
		if cp, _ = ms.MessageEventCheckpoints(ctx); cp != (msgstore.MessageEventCheckpoints{Delivery: 6, Attention: 2, Retry: 0}) {
			t.Fatalf("checkpoints %+v", cp)
		}
		pending, _ := es.Pending(ctx, 0, 0)
		if len(pending) != 3 || pending[0].Kind != "message.read" || pending[1].Kind != "message.sent" {
			t.Fatalf("outbox %+v", pending)
		}
		if ids, _ := ms.DeferredTransitionIDs(ctx, 0, 10); len(ids) != 1 || ids[0] != 6 {
			t.Fatalf("deferred %v", ids)
		}
		if n, _ := ms.CommitMessageEvents(ctx, batch, es); n != 0 {
			t.Fatalf("stale batch committed %d", n)
		}
		retry := msgstore.DrainBatch{From: cp, Ready: []msgstore.DrainRecord{{Position: 6, Events: []eventstore.Row{lifecycle("message.consumed")}}}, Pending: []int64{6}}
		if n, err := ms.CommitMessageEvents(ctx, retry, es); err != nil || n != 1 {
			t.Fatalf("ready %d %v", n, err)
		}
		if ids, _ := ms.DeferredTransitionIDs(ctx, 0, 10); len(ids) != 0 {
			t.Fatalf("deferred after ready %v", ids)
		}
		cp, _ = ms.MessageEventCheckpoints(ctx)
		if cp.Retry != 6 {
			t.Fatalf("retry checkpoint %+v", cp)
		}
		retry.From = cp
		if n, err := ms.CommitMessageEvents(ctx, retry, es); err != nil || n != 0 {
			t.Fatalf("ready again %d %v", n, err)
		}
		if pending, _ = es.Pending(ctx, 0, 0); len(pending) != 4 {
			t.Fatalf("outbox after retries %d", len(pending))
		}

		// Concurrent drainers with the same snapshot: one commits, the rest commit nothing.
		cp, _ = ms.MessageEventCheckpoints(ctx)
		race1 := msgstore.DrainBatch{From: cp, Attention: []msgstore.DrainRecord{{Position: 10, At: t0, Events: []eventstore.Row{lifecycle("message.read")}}}}
		warmPool(ctx, s)
		counts, errs := race(6, func() (int, error) { return ms.CommitMessageEvents(ctx, race1, es) })
		total := 0
		for i, n := range counts {
			if errs[i] != nil {
				t.Fatalf("concurrent commit: %v", errs[i])
			}
			total += n
		}
		if pending, _ = es.Pending(ctx, 0, 0); total != 1 || len(pending) != 5 {
			t.Fatalf("concurrent commits %d, outbox %d", total, len(pending))
		}

		// Concurrent drainers publishing the same ready deferred record, with the retry checkpoint
		// not moving (Pending ends where it already is): every drainer passes the checkpoint check, so
		// only the deferred row's DELETE (RowsAffected) keeps the record from publishing twice.
		cp, _ = ms.MessageEventCheckpoints(ctx)
		exec(t, s, `INSERT INTO message_event_deferred (transition_id) VALUES (?)`, int64(7))
		ready := msgstore.DrainBatch{From: cp, Ready: []msgstore.DrainRecord{{Position: 7, Events: []eventstore.Row{lifecycle("message.consumed")}}},
			Pending: []int64{cp.Retry}}
		warmPool(ctx, s)
		counts, errs = race(6, func() (int, error) { return ms.CommitMessageEvents(ctx, ready, es) })
		total = 0
		for i, n := range counts {
			if errs[i] != nil {
				t.Fatalf("concurrent ready: %v", errs[i])
			}
			total += n
		}
		if pending, _ = es.Pending(ctx, 0, 0); total != 1 || len(pending) != 6 {
			t.Fatalf("concurrent ready publishes %d, outbox %d (want 1, 6)", total, len(pending))
		}
	})
}

func TestAttentionJournal(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms, clock := newStore(s)
		m := send(t, ms, clock, alice, bob)
		if err := ms.RecordMessageWake(ctx, m.ID, bob, "wake.outcome", "", 0, 0, true, true, "busy"); err != nil {
			t.Fatal(err)
		}
		rows, err := ms.AttentionJournal(ctx, 0, 10)
		if err != nil || len(rows) == 0 || rows[len(rows)-1].Kind != "wake.outcome" || rows[len(rows)-1].At.IsZero() {
			t.Fatalf("journal %+v %v", rows, err)
		}
		last := rows[len(rows)-1].ID
		if rows, _ = ms.AttentionJournal(ctx, last, 10); len(rows) != 0 {
			t.Fatalf("after last %+v", rows)
		}
		exec(t, s, `INSERT INTO message_event_deferred (transition_id) VALUES (3), (7), (9)`)
		if ids, _ := ms.DeferredTransitionIDs(ctx, 5, 10); len(ids) != 3 || ids[0] != 7 || ids[1] != 9 || ids[2] != 3 {
			t.Fatalf("deferred order %v", ids)
		}
	})
}

// TestDeliveryLibrary exercises the library-backed half: SQLite only (go-messaging has no Postgres
// store, CW-20261115-0013), where Postgres returns ErrLibraryUnavailable.
func TestDeliveryLibrary(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms, clock := newStore(s)
		if s.C().Dialect().Name() == gostore.DriverPostgres {
			for name, err := range map[string]error{
				"activate": ms.ActivateDelivery(ctx),
				"summary":  func() error { _, err := ms.RetentionSummary(ctx, t0); return err }(),
				"high":     func() error { _, err := ms.DeliveryHighWater(ctx); return err }(),
			} {
				if !errors.Is(err, store.ErrLibraryUnavailable) {
					t.Errorf("%s on postgres: %v", name, err)
				}
			}
		}
		storetest.LibraryDB(t, s) // skips on Postgres
		if err := ms.ActivateDelivery(ctx); err != nil {
			t.Fatal(err)
		}
		if cp, _ := ms.MessageEventCheckpoints(ctx); cp.Delivery != 0 {
			t.Fatalf("activated delivery checkpoint %d", cp.Delivery)
		}
		ds, err := ms.DeliveryStore(ctx)
		if err != nil {
			t.Fatal(err)
		}
		enqueue := func(mailbox bool) (delivery.EnqueueResult, messaging.Envelope) {
			t.Helper()
			res, err := ds.Enqueue(ctx, delivery.EnqueueRequest{From: addr(t, alice), Recipients: []delivery.RecipientTarget{{Address: addr(t, bob)}},
				Kind: messaging.MsgKindRequest, Payload: []byte(`{"subject":"s"}`), Metadata: map[string]string{"urgency": "high"}})
			if err != nil {
				t.Fatal(err)
			}
			var env messaging.Envelope
			if mailbox {
				env, err = ms.SendWithID(ctx, messaging.Envelope{ID: string(res.Message.ID), Kind: messaging.MsgKindRequest, From: addr(t, alice), To: addr(t, bob),
					Payload: []byte(`{"subject":"s"}`)}, string(res.Deliveries[0].ID), false)
				if err != nil {
					t.Fatal(err)
				}
			}
			clock.Advance(time.Second)
			return res, env
		}
		res, env := enqueue(true)
		orphan, _ := enqueue(false)
		if hw, err := ms.DeliveryHighWater(ctx); err != nil || hw != orphan.Deliveries[0].ID {
			t.Fatalf("high water %q %v", hw, err)
		}
		page, err := ms.ReadyDeliveryPage(ctx, "", "", 10)
		if err != nil || len(page.Rows) != 2 || !page.Exhausted || page.LastID != orphan.Deliveries[0].ID {
			t.Fatalf("ready page %+v %v", page, err)
		}
		if page, _ = ms.ReadyDeliveryPage(ctx, "", res.Deliveries[0].ID, 1); len(page.Rows) != 1 || page.Exhausted {
			t.Fatalf("bounded page %+v", page)
		}
		_, lease, err := ms.ClaimMessageDelivery(ctx, env.ID, addr(t, bob), "bridge", time.Minute)
		if err != nil || lease.DeliveryID != res.Deliveries[0].ID {
			t.Fatalf("claim %+v %v", lease, err)
		}
		if _, _, err := ms.AckMessageDelivery(ctx, lease, delivery.StageHostAccepted); err != nil {
			t.Fatal(err)
		}
		cp, _ := ms.MessageEventCheckpoints(ctx)
		trs, err := ms.DeliveryTransitions(ctx, cp.Delivery, msgstore.MessageEventBatch)
		if err != nil || len(trs) < 3 {
			t.Fatalf("transitions %+v %v", trs, err)
		}
		facts, err := ms.TransitionFacts(ctx, trs[0])
		if err != nil || facts.Recipient != bob || facts.MailboxID != env.ID || facts.Stream || facts.Consumed || facts.Message.Metadata["urgency"] != "high" {
			t.Fatalf("facts %+v %v", facts, err)
		}
		var claimed delivery.Transition
		for _, tr := range trs {
			if tr.Reason == delivery.TransitionClaimed {
				claimed = tr
			}
		}
		if facts, err = ms.TransitionFacts(ctx, claimed); err != nil || facts.Attempt != 1 || claimed.AttemptID == "" {
			t.Fatalf("claimed facts %+v %v", facts, err)
		}
		for _, tr := range trs {
			if tr.DeliveryID == orphan.Deliveries[0].ID {
				if _, err := ms.TransitionFacts(ctx, tr); !errors.Is(err, msgstore.ErrMessageContentPending) {
					t.Fatalf("orphan transition: %v", err)
				}
			}
		}
		if one, err := ms.DeliveryTransition(ctx, trs[1].ID); err != nil || one.ID != trs[1].ID {
			t.Fatalf("one transition %+v %v", one, err)
		}
		if _, _, err := ms.NackMessageDelivery(ctx, lease, true, "busy", time.Minute); err != nil {
			t.Fatal(err)
		}
		sum, err := ms.RetentionSummary(ctx, time.Now())
		if err != nil || sum.ByStatus["pending"] != 1 || sum.ByStatus["retry_scheduled"] != 1 || len(sum.Groups) != 4 {
			t.Fatalf("summary %+v %v", sum, err)
		}
		// Reactivating never moves an established checkpoint.
		exec(t, s, `UPDATE message_event_checkpoints SET position = 1 WHERE source = 'delivery'`)
		if err := ms.ActivateDelivery(ctx); err != nil {
			t.Fatal(err)
		}
		if cp, _ = ms.MessageEventCheckpoints(ctx); cp.Delivery != 1 {
			t.Fatalf("reactivated checkpoint %d", cp.Delivery)
		}
	})
}
