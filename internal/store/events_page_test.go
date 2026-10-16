package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

func openEventsStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestEventsPageContextBoundsAndFilters(t *testing.T) {
	db := openEventsStore(t)
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		kind, session := "a", "s1"
		if i%3 == 0 {
			kind = "b"
		}
		if i%5 == 0 {
			session = "s2"
		}
		if _, _, err := db.InsertEventContext(ctx, events.ScopeSession, session, kind, "{}"); err != nil {
			t.Fatal(err)
		}
	}
	lo, hi, err := db.StreamBoundsContext(ctx)
	if err != nil || lo != 1 || hi != 30 {
		t.Fatalf("bounds %d %d %v", lo, hi, err)
	}
	page, err := db.EventsPageContext(ctx, events.Filter{}, 0, hi, 7)
	if err != nil || len(page) != 7 || page[0].Seq != 1 || page[6].Seq != 7 {
		t.Fatalf("first page %v %v", page, err)
	}
	page, _ = db.EventsPageContext(ctx, events.Filter{}, 7, 12, 100)
	if len(page) != 5 || page[4].Seq != 12 {
		t.Fatalf("upTo bound ignored: %d events", len(page))
	}
	page, _ = db.EventsPageContext(ctx, events.Filter{Kinds: []string{"b"}}, 0, hi, 100)
	for _, e := range page {
		if e.Kind != "b" {
			t.Fatalf("kind filter leaked %q", e.Kind)
		}
	}
	if len(page) != 10 {
		t.Fatalf("kind b count %d, want 10", len(page))
	}
	page, _ = db.EventsPageContext(ctx, events.Filter{SessionID: "s2", Kinds: []string{"a", "b"}, Scopes: []events.Scope{events.ScopeSession}}, 0, hi, 100)
	for _, e := range page {
		if e.SessionID != "s2" {
			t.Fatalf("session filter leaked %q", e.SessionID)
		}
	}
	if len(page) != 6 {
		t.Fatalf("session s2 count %d, want 6", len(page))
	}
	if page, _ := db.EventsPageContext(ctx, events.Filter{Scopes: []events.Scope{events.ScopeDaemon}}, 0, hi, 100); len(page) != 0 {
		t.Fatal("scope filter leaked")
	}
	empty := openEventsStore(t)
	if lo, hi, err := empty.StreamBoundsContext(ctx); err != nil || lo != 0 || hi != 0 {
		t.Fatalf("empty bounds %d %d %v", lo, hi, err)
	}
}

// End to end on the real store: a seq-0 subscriber pages the log, a trimmed
// head is announced, and a flood while the consumer is stalled is recovered.
func TestBusOnTheRealStorePagesReplayAndRecoversOverrun(t *testing.T) {
	db := openEventsStore(t)
	ctx := context.Background()
	bus := events.NewBus(events.BusOptions{Persister: db, ReplayPage: 50, SubBuffer: 4, MaxConsecDrops: 1 << 20})
	for i := 0; i < 400; i++ {
		if err := bus.Publish(ctx, events.Event{Scope: events.ScopeSession, SessionID: "s1", Kind: "k"}); err != nil {
			t.Fatal(err)
		}
	}
	ch, cancel, err := bus.Subscribe(ctx, events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	var last int64
	timeout := time.After(10 * time.Second)
	for n := 0; n < 400; {
		select {
		case e := <-ch:
			if e.Kind == events.KindStreamGap {
				t.Fatalf("unexpected gap during a clean replay: %s", e.PayloadJSON)
			}
			if e.Seq != last+1 {
				t.Fatalf("seq %d after %d", e.Seq, last)
			}
			last, n = e.Seq, n+1
		case <-timeout:
			t.Fatalf("replay stalled at %d", last)
		}
	}
	// stall, flood, then drain: gap(overrun, recovered) and no missing seq
	for i := 0; i < 200; i++ {
		if err := bus.Publish(ctx, events.Event{Scope: events.ScopeSession, SessionID: "s1", Kind: "k"}); err != nil {
			t.Fatal(err)
		}
	}
	for last < 600 {
		select {
		case e := <-ch:
			if e.Kind == events.KindStreamGap {
				continue
			}
			if e.Seq != last+1 {
				t.Fatalf("seq %d after %d: events lost across an overrun", e.Seq, last)
			}
			last = e.Seq
		case <-timeout:
			t.Fatalf("recovery stalled at %d", last)
		}
	}
	// head trim: a subscriber behind the trim point is told
	if _, err := db.db.ExecContext(ctx, `DELETE FROM events WHERE id <= 100`); err != nil {
		t.Fatal(err)
	}
	trimmed, cancel2, err := bus.Subscribe(ctx, events.Filter{SinceSeq: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel2()
	select {
	case e := <-trimmed:
		if e.Kind != events.KindStreamGap {
			t.Fatalf("first event %q, want a trimmed gap", e.Kind)
		}
	case <-timeout:
		t.Fatal("no trimmed gap")
	}
}
