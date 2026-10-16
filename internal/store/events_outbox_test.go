package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

func newOutboxBus(t *testing.T, db *Store, opts ...func(*events.BusOptions)) events.OutboxBus {
	t.Helper()
	o := events.BusOptions{Persister: db, OutboxSweep: time.Hour}
	for _, f := range opts {
		f(&o)
	}
	bus, ok := events.NewBus(o).(events.OutboxBus)
	if !ok {
		t.Fatal("the bus is not an OutboxBus")
	}
	return bus
}

func collect(t *testing.T, ch <-chan events.Event, n int, within time.Duration) []events.Event {
	t.Helper()
	var got []events.Event
	deadline := time.After(within)
	for len(got) < n {
		select {
		case e, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed after %d of %d", len(got), n)
			}
			if e.Kind == events.KindStreamGap {
				continue
			}
			got = append(got, e)
		case <-deadline:
			t.Fatalf("got %d of %d events within %s", len(got), n, within)
		}
	}
	return got
}

func expectNone(t *testing.T, ch <-chan events.Event, wait time.Duration) {
	t.Helper()
	select {
	case e := <-ch:
		if e.Kind != events.KindStreamGap {
			t.Fatalf("unexpected event %+v", e)
		}
	case <-time.After(wait):
	}
}

func pendingCount(t *testing.T, db *Store) int {
	t.Helper()
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM events WHERE pending = 1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// PublishTx writes on the caller's transaction; a rollback leaves no row and
// publishes nothing.
func TestPublishTxRollbackPublishesNothing(t *testing.T) {
	db := openEventsStore(t)
	bus := newOutboxBus(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := bus.StartOutbox(ctx)
	defer stop()
	live, unsub, err := bus.(events.LiveSubscriber).SubscribeLive(ctx, events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := bus.PublishTx(ctx, tx, events.Event{Scope: events.ScopeSession, SessionID: "s1", Kind: "session.reaper", PayloadJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	if stored.Seq == 0 || !strings.HasPrefix(stored.EventID, "evt_") || len(stored.EventID) != 36 {
		t.Fatalf("stored %+v", stored)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	bus.KickOutbox()
	expectNone(t, live, 300*time.Millisecond)
	var rows int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&rows); err != nil || rows != 0 || pendingCount(t, db) != 0 {
		t.Fatalf("rollback left rows=%d pending=%d err=%v", rows, pendingCount(t, db), err)
	}
}

// After the commit the row is published exactly once, with its D6 id, and ahead
// of an event another publisher wrote after it.
func TestPublishTxCommitPublishesOnceInOrder(t *testing.T) {
	db := openEventsStore(t)
	bus := newOutboxBus(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := bus.StartOutbox(ctx)
	defer stop()
	live, unsub, err := bus.(events.LiveSubscriber).SubscribeLive(ctx, events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := bus.PublishTx(ctx, tx, events.Event{Scope: events.ScopeSession, SessionID: "s1", Kind: "session.reaper", PayloadJSON: `{"a":1}`})
	if err != nil {
		t.Fatal(err)
	}
	// A normal publisher races the commit: it must wait for the connection and
	// then be ordered after the outbox row.
	pubDone := make(chan error, 1)
	go func() {
		pubDone <- bus.Publish(ctx, events.Event{Scope: events.ScopeSession, SessionID: "s1", Kind: "later"})
	}()
	time.Sleep(50 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	db.EventsCommitted() // no hook installed: harmless
	bus.KickOutbox()
	if err := <-pubDone; err != nil {
		t.Fatal(err)
	}
	got := collect(t, live, 2, 5*time.Second)
	if got[0].Kind != "session.reaper" || got[1].Kind != "later" || got[0].Seq >= got[1].Seq {
		t.Fatalf("order/kinds: %+v", got)
	}
	if got[0].EventID != stored.EventID || got[0].Seq != stored.Seq || got[1].EventID == "" || got[0].EventID == got[1].EventID {
		t.Fatalf("event ids: %q vs stored %q, later %q", got[0].EventID, stored.EventID, got[1].EventID)
	}
	expectNone(t, live, 300*time.Millisecond) // not published twice
	if pendingCount(t, db) != 0 {
		t.Fatal("rows still pending after publish")
	}
}

// One database connection: a transaction holding it must be able to PublishTx
// while another goroutine holds the bus lock waiting for that connection.
func TestPublishTxDoesNotDeadlockWithTheSingleConnection(t *testing.T) {
	db := openEventsStore(t)
	if got := db.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("test premise: MaxOpenConns=%d, want 1", got)
	}
	bus := newOutboxBus(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Another publisher takes the bus lock and blocks on the connection tx holds.
	started := make(chan struct{})
	pubDone := make(chan error, 1)
	go func() {
		close(started)
		pubDone <- bus.Publish(ctx, events.Event{Scope: events.ScopeDaemon, Kind: "blocked"})
	}()
	<-started
	time.Sleep(100 * time.Millisecond) // it now holds the bus lock, waiting for the connection

	done := make(chan error, 1)
	go func() {
		_, err := bus.PublishTx(ctx, tx, events.Event{Scope: events.ScopeSession, SessionID: "s", Kind: "session.reaper"})
		if err == nil {
			err = tx.Commit()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: PublishTx did not complete while a publisher held the bus lock")
	}
	if err := <-pubDone; err != nil {
		t.Fatal(err)
	}
}

// Crash between commit and publish: the committed row is still published, once,
// by the next process's drainer; a later restart does not publish it again.
func TestCrashBetweenCommitAndPublishStillPublishesOnce(t *testing.T) {
	db := openEventsStore(t)
	ctx := context.Background()
	first := newOutboxBus(t, db)
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := first.PublishTx(ctx, tx, events.Event{Scope: events.ScopeSession, SessionID: "s1", Kind: "session.reaper", PayloadJSON: `{"x":1}`})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// the process dies here: no kick, no drain
	if pendingCount(t, db) != 1 {
		t.Fatal("test premise: the row should be pending")
	}

	second := newOutboxBus(t, db) // the restarted process
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	live, unsub, err := second.(events.LiveSubscriber).SubscribeLive(rctx, events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	stop := second.StartOutbox(rctx)
	defer stop()
	got := collect(t, live, 1, 5*time.Second)
	if got[0].EventID != stored.EventID || got[0].Seq != stored.Seq || got[0].Kind != "session.reaper" {
		t.Fatalf("published %+v, want %+v", got[0], stored)
	}
	expectNone(t, live, 300*time.Millisecond)
	if pendingCount(t, db) != 0 {
		t.Fatal("still pending after the drain")
	}

	third := newOutboxBus(t, db) // another restart: nothing left to publish
	live3, unsub3, err := third.(events.LiveSubscriber).SubscribeLive(rctx, events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub3()
	stop3 := third.StartOutbox(rctx)
	defer stop3()
	expectNone(t, live3, 300*time.Millisecond)
}

// A crash after the fan-out but before the mark publishes again (at least once);
// the repeat carries the same event id, which is the dedupe key.
type failFirstMark struct {
	*Store
	failed atomic.Bool
}

func (f *failFirstMark) MarkPublishedContext(ctx context.Context, seqs []int64) error {
	if f.failed.CompareAndSwap(false, true) {
		return errors.New("crash before the mark")
	}
	return f.Store.MarkPublishedContext(ctx, seqs)
}

func TestOutboxIsAtLeastOnceAndRepeatsCarryTheSameEventID(t *testing.T) {
	db := openEventsStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	flaky := &failFirstMark{Store: db}
	bus := events.NewBus(events.BusOptions{Persister: flaky, OutboxSweep: 20 * time.Millisecond}).(events.OutboxBus)
	live, unsub, err := bus.(events.LiveSubscriber).SubscribeLive(ctx, events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := bus.PublishTx(ctx, tx, events.Event{Scope: events.ScopeSession, SessionID: "s", Kind: "session.reaper"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	stop := bus.StartOutbox(ctx)
	defer stop()
	got := collect(t, live, 2, 5*time.Second)
	if got[0].EventID != stored.EventID || got[1].EventID != stored.EventID || got[0].Seq != got[1].Seq {
		t.Fatalf("repeat differs: %+v", got)
	}
	expectNone(t, live, 200*time.Millisecond) // and then it stops
}

// A repeat of an event id already stored is the same row, not a second one.
func TestPublishTxIsIdempotentOnTheEventID(t *testing.T) {
	db := openEventsStore(t)
	bus := newOutboxBus(t, db)
	ctx := context.Background()
	write := func() events.Event {
		tx, err := db.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		e, err := bus.PublishTx(ctx, tx, events.Event{EventID: "evt_" + strings.Repeat("a", 32), Scope: events.ScopeSession, SessionID: "s", Kind: "session.reaper"})
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return e
	}
	a, b := write(), write()
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil || n != 1 || a.Seq != b.Seq {
		t.Fatalf("rows=%d seq %d/%d err=%v", n, a.Seq, b.Seq, err)
	}
}

// Without a kick the sweep still finds committed rows.
func TestOutboxSweepFindsRowsNobodyKickedAbout(t *testing.T) {
	db := openEventsStore(t)
	bus := newOutboxBus(t, db, func(o *events.BusOptions) { o.OutboxSweep = 20 * time.Millisecond })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := bus.StartOutbox(ctx)
	defer stop()
	live, unsub, err := bus.(events.LiveSubscriber).SubscribeLive(ctx, events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bus.PublishTx(ctx, tx, events.Event{Scope: events.ScopeSession, SessionID: "s", Kind: "session.reaper"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	collect(t, live, 1, 5*time.Second)
}

// Rows that predate the outbox have no event id and are never pending, so the
// drainer never re-publishes them; a normal publish now mints and persists an id.
func TestLegacyRowsAreLeftAloneAndNormalPublishMintsAnID(t *testing.T) {
	db := openEventsStore(t)
	if _, err := db.db.Exec(`INSERT INTO events (scope, session_id, at, kind, payload_json) VALUES ('session','old','2026-01-01T00:00:00Z','legacy','{}')`); err != nil {
		t.Fatal(err)
	}
	bus := newOutboxBus(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := bus.StartOutbox(ctx)
	defer stop()
	live, unsub, err := bus.(events.LiveSubscriber).SubscribeLive(ctx, events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	expectNone(t, live, 200*time.Millisecond)
	if err := bus.Publish(ctx, events.Event{Scope: events.ScopeSession, SessionID: "s", Kind: "fresh"}); err != nil {
		t.Fatal(err)
	}
	got := collect(t, live, 1, 5*time.Second)
	if !strings.HasPrefix(got[0].EventID, "evt_") {
		t.Fatalf("fanned-out event has no id: %+v", got[0])
	}
	var stored sql.NullString
	if err := db.db.QueryRow(`SELECT event_id FROM events WHERE kind='fresh'`).Scan(&stored); err != nil || stored.String != got[0].EventID {
		t.Fatalf("persisted id %q vs fanned-out %q (%v)", stored.String, got[0].EventID, err)
	}
	var legacyID sql.NullString
	if err := db.db.QueryRow(`SELECT event_id FROM events WHERE kind='legacy'`).Scan(&legacyID); err != nil || legacyID.Valid {
		t.Fatal("a legacy row was given an id (no backfill is allowed)")
	}
	if pendingCount(t, db) != 0 {
		t.Fatal("legacy rows became pending")
	}
}

// The environment-request kind keeps its own status bookkeeping and is refused here.
func TestPublishTxRefusesTheEnvironmentRequestKind(t *testing.T) {
	db := openEventsStore(t)
	bus := newOutboxBus(t, db)
	ctx := context.Background()
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := bus.PublishTx(ctx, tx, events.Event{Scope: events.ScopeSession, SessionID: "s", Kind: EnvironmentRequestKind, PayloadJSON: `{}`}); err == nil {
		t.Fatal("environment request kind accepted on a caller's tx")
	}
	if _, err := bus.PublishTx(ctx, tx, events.Event{Kind: "x"}); !errors.Is(err, events.ErrScopeEmpty) {
		t.Fatalf("empty scope: %v", err)
	}
}

// The hole in "drain first": a publisher takes the bus lock, sees nothing dirty
// and blocks on the connection a caller's transaction holds; the transaction then
// writes an outbox row and commits. The publisher's own insert gets the higher
// Seq, yet the committed row must still reach a cursor subscriber first (a paged
// subscriber advances past every Seq it sees and would drop the lower one).
func TestPublishStartedBeforePublishTxStillDeliversInSeqOrder(t *testing.T) {
	db := openEventsStore(t)
	bus := newOutboxBus(t, db)
	ctx := context.Background()
	// no StartOutbox: ordering must come from Publish itself
	sub, unsub, err := bus.Subscribe(ctx, events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	// get past the startup drain so the dirty flag is clean when Publish looks
	if err := bus.Publish(ctx, events.Event{Scope: events.ScopeDaemon, Kind: "warmup"}); err != nil {
		t.Fatal(err)
	}
	collect(t, sub, 1, 5*time.Second)

	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	pubDone := make(chan error, 1)
	go func() {
		pubDone <- bus.Publish(ctx, events.Event{Scope: events.ScopeSession, SessionID: "s", Kind: "later"})
	}()
	time.Sleep(150 * time.Millisecond) // Publish holds the bus lock, saw nothing dirty, waits for the connection
	stored, err := bus.PublishTx(ctx, tx, events.Event{Scope: events.ScopeSession, SessionID: "s", Kind: "session.reaper"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-pubDone; err != nil {
		t.Fatal(err)
	}
	got := collect(t, sub, 2, 5*time.Second)
	if got[0].Kind != "session.reaper" || got[0].Seq != stored.Seq || got[1].Kind != "later" || got[0].Seq >= got[1].Seq {
		t.Fatalf("delivered %+v then %+v (outbox row seq %d)", got[0], got[1], stored.Seq)
	}
	expectNone(t, sub, 300*time.Millisecond)
	if pendingCount(t, db) != 0 {
		t.Fatal("outbox row still pending")
	}
}

// Rows a crashed process left pending are not overtaken by the first Publish of
// the next process, even before StartOutbox runs.
func TestFirstPublishAfterRestartDeliversLeftoverRowsFirst(t *testing.T) {
	db := openEventsStore(t)
	ctx := context.Background()
	first := newOutboxBus(t, db)
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := first.PublishTx(ctx, tx, events.Event{Scope: events.ScopeSession, SessionID: "s", Kind: "session.reaper"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	second := newOutboxBus(t, db) // restarted; StartOutbox not called yet
	live, unsub, err := second.(events.LiveSubscriber).SubscribeLive(ctx, events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	if err := second.Publish(ctx, events.Event{Scope: events.ScopeDaemon, Kind: "normal"}); err != nil {
		t.Fatal(err)
	}
	got := collect(t, live, 2, 5*time.Second)
	if got[0].Seq != stored.Seq || got[1].Kind != "normal" || got[0].Seq >= got[1].Seq {
		t.Fatalf("order: %+v", got)
	}
}

// A repeat PublishTx hands back the stored row, not what the second caller sent.
func TestRepeatPublishTxReturnsTheStoredRow(t *testing.T) {
	db := openEventsStore(t)
	bus := newOutboxBus(t, db)
	ctx := context.Background()
	id := "evt_" + strings.Repeat("b", 32)
	write := func(payload string, at time.Time) events.Event {
		tx, err := db.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		e, err := bus.PublishTx(ctx, tx, events.Event{EventID: id, At: at, Scope: events.ScopeSession, SessionID: "s", Kind: "session.reaper", PayloadJSON: payload})
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return e
	}
	first := write(`{"n":1}`, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	second := write(`{"n":2}`, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if second.Seq != first.Seq || second.PayloadJSON != `{"n":1}` || !second.At.Equal(first.At) {
		t.Fatalf("repeat returned %+v, want the stored row %+v", second, first)
	}
}

// Every read path carries the event id (replays and queries included).
func TestStoredEventsAreReadBackWithTheirEventID(t *testing.T) {
	db := openEventsStore(t)
	ctx := context.Background()
	_, _, id, err := db.InsertEventIDContext(ctx, events.ScopeSession, "s", "k", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	since, err := db.EventsSinceContext(ctx, 0)
	if err != nil || len(since) != 1 || since[0].EventID != id {
		t.Fatalf("EventsSinceContext: %+v %v", since, err)
	}
	bySession, err := db.ListEventsBySession("s", 10, 0)
	if err != nil || len(bySession) != 1 || bySession[0].EventID != id {
		t.Fatalf("ListEventsBySession: %+v %v", bySession, err)
	}
	recent, err := db.ListRecentEvents(10)
	if err != nil || len(recent) != 1 || recent[0].EventID != id {
		t.Fatalf("ListRecentEvents: %+v %v", recent, err)
	}
	queried, err := db.QueryEvents(EventFilter{SessionID: "s"})
	if err != nil || len(queried) != 1 || queried[0].EventID != id {
		t.Fatalf("QueryEvents: %+v %v", queried, err)
	}
}
