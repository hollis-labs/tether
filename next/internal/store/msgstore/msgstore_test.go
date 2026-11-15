package msgstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/msgstore"
	"github.com/hollis-labs/tether/next/internal/store/storetest"
)

var t0 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func addr(t *testing.T, urn string) messaging.Address {
	t.Helper()
	a, err := messaging.ParseURN(urn)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func newStore(s *store.Stores) (*msgstore.Store, *storetest.Clock) {
	ms := msgstore.New(s)
	clock := storetest.NewClock(t0)
	ms.Now = clock.Now
	ids := storetest.NewIDs("msg-")
	ms.NewMessageID = func() (string, error) { return ids.New(), nil }
	return ms, clock
}

func exec(t *testing.T, s *store.Stores, q string, args ...any) {
	t.Helper()
	if err := s.C().WriteTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		_, err := tx.ExecContext(ctx, tx.Dialect().Rebind(q), args...)
		return err
	}); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func send(t *testing.T, ms *msgstore.Store, clock *storetest.Clock, from, to string, mut ...func(*messaging.Envelope)) messaging.Envelope {
	t.Helper()
	env := messaging.Envelope{Kind: messaging.MsgKindRequest, From: addr(t, from), To: addr(t, to), Payload: json.RawMessage(`{"subject":"s","body":"b"}`)}
	for _, m := range mut {
		m(&env)
	}
	out, err := ms.Send(context.Background(), env, false)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	return out
}

const (
	alice = "msg://agent/cell/alice"
	bob   = "msg://agent/cell/bob"
	op    = "msg://user/local/operator"
)

func TestSendGetThreadInbox(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms, clock := newStore(s)
		if _, err := ms.Send(ctx, messaging.Envelope{From: addr(t, alice), To: addr(t, bob), DeliveredAt: &t0}, false); !errors.Is(err, messaging.ErrPresetLifecycle) {
			t.Fatalf("preset lifecycle: %v", err)
		}
		a := send(t, ms, clock, alice, bob, func(e *messaging.Envelope) {
			e.ThreadID, e.ContentType, e.Metadata = "th", "application/json", map[string]string{"k": "v"}
		})
		b := send(t, ms, clock, alice, bob, func(e *messaging.Envelope) { e.ThreadID, e.Kind = "th", messaging.MsgKindNotice })
		got, err := ms.Get(ctx, a.ID)
		if err != nil || got.ThreadID != "th" || got.Metadata["k"] != "v" || string(got.Payload) != `{"subject":"s","body":"b"}` || !got.CreatedAt.Equal(t0) {
			t.Fatalf("Get = %+v %v", got, err)
		}
		if _, err := ms.Get(ctx, "nope"); !errors.Is(err, messaging.ErrNotFound) {
			t.Fatalf("Get missing: %v", err)
		}
		thread, _ := ms.Thread(ctx, "th", messaging.Filter{})
		if len(thread) != 2 || thread[0].ID != a.ID {
			t.Fatalf("Thread %v", thread)
		}
		if notices, _ := ms.Thread(ctx, "th", messaging.Filter{Kind: []messaging.Kind{messaging.MsgKindNotice}}); len(notices) != 1 || notices[0].ID != b.ID {
			t.Fatalf("Thread kinds %v", notices)
		}
		// A staged row never appears.
		exec(t, s, `INSERT INTO messages (id, kind, from_urn, to_urn, created_at, routing_staged) VALUES ('staged-1', 'request', ?, ?, ?, 1)`, alice, bob, "2026-11-14T11:00:00Z")
		if err := ms.MarkRead(ctx, b.ID, addr(t, bob)); err != nil {
			t.Fatal(err)
		}
		unread, _ := ms.Inbox(ctx, addr(t, bob), messaging.Filter{}, true)
		if len(unread) != 1 || unread[0].ID != a.ID {
			t.Fatalf("Inbox unreadOnly %v", unread)
		}
		all, _ := ms.Inbox(ctx, addr(t, bob), messaging.Filter{}, false)
		if len(all) != 1 || all[0].ID != b.ID {
			t.Fatalf("Inbox after delivery %v", all)
		}
		if again, _ := ms.Inbox(ctx, addr(t, bob), messaging.Filter{}, false); len(again) != 0 {
			t.Fatalf("Inbox delivered twice: %v", again)
		}
		if got, _ := ms.Get(ctx, a.ID); got.DeliveredAt == nil {
			t.Fatal("delivered_at not stamped")
		}
	})
}

func TestConsumeCancelStamps(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms, clock := newStore(s)
		m := send(t, ms, clock, alice, bob)
		withDelivery, err := ms.SendWithID(ctx, messaging.Envelope{ID: "fixed-1", Kind: messaging.MsgKindRequest, From: addr(t, alice), To: addr(t, bob)}, "dlv-9", false)
		if err != nil {
			t.Fatal(err)
		}
		if id, err := ms.Consume(ctx, withDelivery.ID, addr(t, bob)); err != nil || id != "dlv-9" {
			t.Fatalf("Consume delivery id %q %v", id, err)
		}
		if id, err := ms.Consume(ctx, withDelivery.ID, addr(t, bob)); err != nil || id != "dlv-9" {
			t.Fatalf("Consume repeat %q %v", id, err)
		}
		if _, err := ms.Consume(ctx, m.ID, addr(t, alice)); !errors.Is(err, msgstore.ErrWrongRecipient) {
			t.Fatalf("Consume by another: %v", err)
		}
		if _, err := ms.Consume(ctx, "nope", addr(t, bob)); !errors.Is(err, messaging.ErrNotFound) {
			t.Fatalf("Consume missing: %v", err)
		}
		if err := ms.Cancel(ctx, m.ID); err != nil {
			t.Fatal(err)
		}
		if err := ms.Cancel(ctx, m.ID); err != nil {
			t.Fatalf("Cancel repeat: %v", err)
		}
		if err := ms.Cancel(ctx, "nope"); !errors.Is(err, messaging.ErrNotFound) {
			t.Fatalf("Cancel missing: %v", err)
		}
		for name, fn := range map[string]func(context.Context, string, messaging.Address) error{"MarkRead": ms.MarkRead, "Archive": ms.Archive, "Unarchive": ms.Unarchive} {
			if err := fn(ctx, m.ID, addr(t, bob)); err != nil {
				t.Errorf("%s: %v", name, err)
			}
			if err := fn(ctx, m.ID, addr(t, bob)); err != nil {
				t.Errorf("%s repeat: %v", name, err)
			}
			if err := fn(ctx, m.ID, addr(t, alice)); !errors.Is(err, msgstore.ErrWrongRecipient) {
				t.Errorf("%s by another: %v", name, err)
			}
		}
		// A channel publication has no recipient lifecycle.
		pub, err := ms.Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, Channel: "announce", From: addr(t, alice), To: addr(t, "msg://service/local/channel/announce")}, true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ms.Consume(ctx, pub.ID, addr(t, "msg://service/local/channel/announce")); !errors.Is(err, msgstore.ErrPublicationNotMailbox) {
			t.Fatalf("Consume publication: %v", err)
		}
	})
}

func TestListPageAndWatch(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms, clock := newStore(s)
		m1 := send(t, ms, clock, alice, bob)
		m2 := send(t, ms, clock, op, bob, func(e *messaging.Envelope) { e.Kind = messaging.MsgKindNotice })
		m3 := send(t, ms, clock, bob, op)
		_ = ms.Archive(ctx, m1.ID, addr(t, bob))
		page, err := ms.List(ctx, addr(t, bob), msgstore.ListFilter{})
		if err != nil || page.Total != 1 || page.Messages[0].ID != m2.ID || page.Messages[0].Subject != "s" || page.Messages[0].Body != "b" {
			t.Fatalf("List %+v %v", page, err)
		}
		if page, _ = ms.List(ctx, addr(t, bob), msgstore.ListFilter{IncludeArchived: true, Limit: 1}); page.Total != 2 || len(page.Messages) != 1 || page.Messages[0].ID != m2.ID {
			t.Fatalf("List archived %+v", page)
		}
		op1, err := ms.ListMessagesPage(ctx, msgstore.MessageQuery{Scope: "user"})
		if err != nil || op1.Total != 1 || op1.Messages[0].ID != m3.ID {
			t.Fatalf("ListMessagesPage user %+v %v", op1, err)
		}
		if p, _ := ms.ListMessagesPage(ctx, msgstore.MessageQuery{Archive: "archived"}); p.Total != 1 || p.Messages[0].ID != m1.ID {
			t.Fatalf("ListMessagesPage archived %+v", p)
		}
		if _, err := ms.ListMessagesPage(ctx, msgstore.MessageQuery{Scope: "everyone"}); err == nil {
			t.Fatal("invalid scope accepted")
		}
		win, err := ms.MailboxAfter(ctx, addr(t, bob), 0, msgstore.ListFilter{IncludeArchived: true}, 0)
		if err != nil || len(win.Entries) != 2 || win.HighWater < win.Entries[1].Seq || win.Entries[0].Seq >= win.Entries[1].Seq {
			t.Fatalf("MailboxAfter %+v %v", win, err)
		}
		next, _ := ms.MailboxAfter(ctx, addr(t, bob), win.Entries[0].Seq, msgstore.ListFilter{IncludeArchived: true}, 0)
		if len(next.Entries) != 1 || next.Entries[0].ID != m2.ID {
			t.Fatalf("MailboxAfter cursor %+v", next)
		}
	})
}

func TestAliasesPayloadsAndReplies(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms, clock := newStore(s)
		if err := ms.SetMessageAlias(ctx, bob, "Bob"); err != nil {
			t.Fatal(err)
		}
		if err := ms.SetMessageAlias(ctx, alice, "alice"); err != nil {
			t.Fatal(err)
		}
		if err := ms.SetMessageAlias(ctx, bob, "bobby"); err != nil {
			t.Fatal(err)
		}
		if err := ms.SetMessageAlias(ctx, bob, "1bad"); err == nil {
			t.Fatal("invalid alias accepted")
		}
		list, _ := ms.ListMessageAliases(ctx)
		if !reflect.DeepEqual(list, []msgstore.MessageAlias{{URN: alice, Alias: "alice"}, {URN: bob, Alias: "bobby"}}) {
			t.Fatalf("aliases %+v", list)
		}
		if a, err := ms.ResolveMessageAddress(ctx, "@BOBBY"); err != nil || a.URN() != bob {
			t.Fatalf("resolve alias %v %v", a, err)
		}
		if _, err := ms.ResolveMessageAddress(ctx, "nobody"); err == nil {
			t.Fatal("unknown alias resolved")
		}
		// NOCASE folds ASCII only: U+212A KELVIN SIGN is not k.
		if err := ms.SetMessageAlias(ctx, alice, "kelvin"); err != nil {
			t.Fatal(err)
		}
		if a, err := ms.ResolveMessageAddress(ctx, "\u212Aelvin"); err == nil {
			t.Fatalf("kelvin sign resolved %v", a)
		}
		if a, err := ms.ResolveMessageAddress(ctx, "KELVIN"); err != nil || a.URN() != alice {
			t.Fatalf("ASCII fold %v %v", a, err)
		}
		const ct = "application/vnd.tether.envelope+json"
		typed := func(payload string) func(*messaging.Envelope) {
			return func(e *messaging.Envelope) { e.ContentType, e.Payload = ct, json.RawMessage(payload) }
		}
		ask := send(t, ms, clock, op, bob, typed(`{"kind":"question","subject":"q1"}`))
		named := send(t, ms, clock, op, bob, typed(`{"kind":"notice","awaiting":"@bob"}`))
		other := send(t, ms, clock, op, bob, typed(`{"kind":"notice","awaiting":"@someone"}`))
		answered := send(t, ms, clock, op, bob, typed(`{"kind":"decision"}`))
		send(t, ms, clock, bob, op, func(e *messaging.Envelope) { e.InReplyTo = answered.ID })
		send(t, ms, clock, op, bob, func(e *messaging.Envelope) { e.ContentType, e.Payload = ct, json.RawMessage(`not json`) })
		archived := send(t, ms, clock, op, bob, typed(`{"kind":"question"}`))
		_ = ms.Archive(ctx, archived.ID, addr(t, bob))
		// A \u0000 escape: valid JSON on both dialects (Postgres's jsonb would refuse it).
		nul := send(t, ms, clock, op, bob, typed(`{"kind":"question","note":"a\u0000b"}`))
		_ = other
		cands, err := ms.PendingReplyCandidates(ctx, []string{bob}, "@bob", ct, []string{"question", "decision", "approval_request"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, c := range cands {
			ids = append(ids, c.MessageID)
		}
		if !reflect.DeepEqual(ids, []string{nul.ID, named.ID, ask.ID}) {
			t.Fatalf("candidates %v, want %v", ids, []string{nul.ID, named.ID, ask.ID})
		}
		if p, ok, _ := ms.PayloadWithContentType(ctx, ask.ID, ct); !ok || p != `{"kind":"question","subject":"q1"}` {
			t.Fatalf("PayloadWithContentType %q %v", p, ok)
		}
		if _, ok, _ := ms.PayloadWithContentType(ctx, ask.ID, "text/plain"); ok {
			t.Fatal("content type not checked")
		}
	})
}

func TestBrokerEnvelopes(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms, _ := newStore(s)
		if err := ms.CreateBrokerEnvelope(ctx, msgstore.BrokerEnvelope{ID: "e"}); err == nil {
			t.Fatal("missing created_at accepted")
		}
		for i, e := range []msgstore.BrokerEnvelope{
			{ID: "e1", Recipient: "r", WorkflowID: "w", CorrelationID: "c1", Priority: 3, Payload: "{}", CreatedAt: "2026-11-14T10:00:00Z", AuditJSON: "[]"},
			{ID: "e2", Recipient: "r", WorkflowID: "w", CorrelationID: "c2", CreatedAt: "2026-11-14T10:00:01Z", DeliveredAt: "2026-11-14T10:00:02Z"},
		} {
			if err := ms.CreateBrokerEnvelope(ctx, e); err != nil {
				t.Fatalf("envelope %d: %v", i, err)
			}
		}
		got, err := ms.GetBrokerEnvelope(ctx, "e1")
		if err != nil || got.Priority != 3 || got.AuditJSON != "[]" || got.Sender != "" {
			t.Fatalf("get %+v %v", got, err)
		}
		if l, _ := ms.ListBrokerEnvelopesByRecipient(ctx, "r"); len(l) != 1 || l[0].ID != "e1" {
			t.Fatalf("by recipient %+v", l)
		}
		if l, _ := ms.ListBrokerEnvelopesByWorkflow(ctx, "w", ""); len(l) != 2 {
			t.Fatalf("by workflow %+v", l)
		}
		if l, _ := ms.ListBrokerEnvelopesByWorkflow(ctx, "w", "c2"); len(l) != 1 || l[0].ID != "e2" {
			t.Fatalf("by correlation %+v", l)
		}
	})
}

func TestRetention(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms, clock := newStore(s)
		consumed := send(t, ms, clock, alice, bob)
		_, _ = ms.Consume(ctx, consumed.ID, addr(t, bob))
		pending := send(t, ms, clock, alice, bob)
		delivered, _ := ms.SendWithID(ctx, messaging.Envelope{ID: "d1", Kind: messaging.MsgKindRequest, From: addr(t, alice), To: addr(t, bob), CreatedAt: t0}, "dlv-ok", false)
		retrying, _ := ms.SendWithID(ctx, messaging.Envelope{ID: "d2", Kind: messaging.MsgKindRequest, From: addr(t, alice), To: addr(t, bob), CreatedAt: t0}, "dlv-retry", false)
		exec(t, s, `INSERT INTO messages (id, kind, from_urn, to_urn, payload, created_at, routing_staged) VALUES ('st-old', 'request', ?, ?, '{}', ?, 1)`,
			alice, bob, t0.Add(-31*24*time.Hour).Format(time.RFC3339Nano))
		status := func(_ context.Context, id string) (string, bool, error) {
			return map[string]string{"dlv-ok": "delivered", "dlv-retry": "retry_scheduled"}[id], true, nil
		}
		clock.Advance(time.Hour)
		cands, err := ms.ListRetentionCandidates(ctx, clock.Now(), status)
		if err != nil {
			t.Fatal(err)
		}
		eligible := map[string]bool{}
		for _, c := range cands {
			eligible[c.MessageID] = c.Eligible
		}
		want := map[string]bool{consumed.ID: true, pending.ID: false, delivered.ID: true, retrying.ID: false, "st-old": true}
		if !reflect.DeepEqual(eligible, want) {
			t.Fatalf("eligibility %v, want %v", eligible, want)
		}
		if _, err := ms.PurgeMessageBody(ctx, pending.ID, op, status); !errors.Is(err, msgstore.ErrPendingObligation) {
			t.Fatalf("purge pending: %v", err)
		}
		missing := func(context.Context, string) (string, bool, error) { return "", false, nil }
		if _, err := ms.PurgeMessageBody(ctx, retrying.ID, op, missing); !errors.Is(err, messaging.ErrNotFound) {
			t.Fatalf("purge with no delivery row: %v", err)
		}
		if _, err := ms.PurgeMessageBody(ctx, consumed.ID, "not a urn", status); err == nil {
			t.Fatal("bad authorized_by accepted")
		}
		for _, id := range []string{consumed.ID, delivered.ID, "st-old"} {
			if ok, err := ms.PurgeMessageBody(ctx, id, op, status); err != nil || ok != (id != delivered.ID) {
				// delivered.ID was sent without a payload or metadata, so nothing is left to clear.
				t.Fatalf("purge %s: %v %v", id, ok, err)
			}
		}
		if ok, _ := ms.PurgeMessageBody(ctx, consumed.ID, op, status); ok {
			t.Fatal("second purge reported a change")
		}
		var audits int
		r := s.C().Reader()
		if err := r.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_purge_audit`).Scan(&audits); err != nil || audits != 2 {
			t.Fatalf("purge audits %d %v", audits, err)
		}
		if got, _ := ms.Get(ctx, consumed.ID); got.Payload != nil {
			t.Fatalf("payload survived: %s", got.Payload)
		}
		if _, err := ms.PurgeMessageBody(ctx, "missing", op, status); !errors.Is(err, messaging.ErrNotFound) {
			t.Fatalf("purge missing: %v", err)
		}
	})
}

// TestSeqIsMonotonic: concurrent senders get distinct, increasing seq values, and a watcher that
// reads the window up to the high-water mark sees every message exactly once (the reason for
// LockMessageSeq on Postgres).
func TestSeqIsMonotonic(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms := msgstore.New(s)
		const n = 40
		errs := make(chan error, n)
		for i := 0; i < n; i++ {
			go func(i int) {
				_, err := ms.Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: addr(t, alice), To: addr(t, bob), Payload: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i))}, false)
				errs <- err
			}(i)
		}
		for i := 0; i < n; i++ {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		seen, after := map[int64]bool{}, int64(0)
		for {
			win, err := ms.MailboxAfter(ctx, addr(t, bob), after, msgstore.ListFilter{}, 7)
			if err != nil {
				t.Fatal(err)
			}
			if len(win.Entries) == 0 {
				break
			}
			for _, e := range win.Entries {
				if seen[e.Seq] || e.Seq <= after {
					t.Fatalf("seq %d out of order or repeated (after %d)", e.Seq, after)
				}
				seen[e.Seq], after = true, e.Seq
			}
		}
		if len(seen) != n {
			t.Fatalf("watched %d of %d messages", len(seen), n)
		}
	})
}

// TestInboxDeliversOnce runs concurrent destructive pulls for one recipient: every message is
// returned exactly once across them (on Postgres they overlap; on SQLite the writer serializes them).
func TestInboxDeliversOnce(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms, clock := newStore(s)
		const total = 60
		for range total {
			send(t, ms, clock, op, bob)
		}
		const pullers = 6
		got := make(chan []messaging.Envelope, pullers)
		errs := make(chan error, pullers)
		var wg sync.WaitGroup
		for range pullers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				envs, err := ms.Inbox(ctx, addr(t, bob), messaging.Filter{}, false)
				got <- envs
				errs <- err
			}()
		}
		wg.Wait()
		close(got)
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		seen := map[string]int{}
		for envs := range got {
			for _, e := range envs {
				seen[e.ID]++
			}
		}
		rest, err := ms.Inbox(ctx, addr(t, bob), messaging.Filter{}, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range rest {
			seen[e.ID]++
		}
		if len(seen) != total {
			t.Fatalf("delivered %d distinct messages, want %d", len(seen), total)
		}
		for id, n := range seen {
			if n != 1 {
				t.Fatalf("message %s delivered %d times", id, n)
			}
		}
	})
}

// TestJournalIDsCommitInOrder holds a transaction open after a message state change wrote its
// journal row (the schema trigger), while RecordMessageWake writes another: the drain reads the
// journal by id > checkpoint, so the second (higher id) must not be visible before the first
// commits. On Postgres the identity alone allows it; migration 00005's statement lock prevents it.
func TestJournalIDsCommitInOrder(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms, clock := newStore(s)
		m := send(t, ms, clock, alice, bob)
		var base int
		r := s.C().Reader()
		if err := r.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_event_journal`).Scan(&base); err != nil {
			t.Fatal(err)
		}
		inserted, release := make(chan struct{}), make(chan struct{})
		first := make(chan error, 1)
		go func() {
			first <- s.C().WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
				if _, err := tx.ExecContext(ctx, tx.Dialect().Rebind(`UPDATE messages SET read_at = ? WHERE id = ?`), "2026-11-15T00:00:00Z", m.ID); err != nil {
					return err
				}
				close(inserted)
				select {
				case <-release:
				case <-time.After(5 * time.Second):
				}
				return nil
			})
		}()
		<-inserted
		second := make(chan error, 1)
		go func() { second <- ms.RecordMessageWake(ctx, m.ID, bob, "wake.outcome", "", 0, 0, true, true, "busy") }()
		time.Sleep(300 * time.Millisecond)
		var n int
		if err := r.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_event_journal`).Scan(&n); err != nil || n != base {
			t.Errorf("while the first journal insert is open, %d new rows are visible (%v): a later id committed first", n-base, err)
		}
		close(release)
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		if err := <-second; err != nil {
			t.Fatal(err)
		}
		rows, err := r.QueryContext(ctx, `SELECT kind FROM message_event_journal ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var kinds []string
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				t.Fatal(err)
			}
			kinds = append(kinds, k)
		}
		if len(kinds) != base+2 || kinds[base] != "message.read" || kinds[base+1] != "wake.outcome" {
			t.Fatalf("journal after both commit: %v", kinds[base:])
		}
	})
}
