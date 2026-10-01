package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

func retentionService(t *testing.T, rc config.EventsRetentionConfig) (*Service, events.Bus) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cat := &config.Catalog{}
	cat.Global.Daemon.EventsRetention = rc
	return &Service{Store: db, Catalog: cat}, events.NewBus(events.BusOptions{Persister: db})
}

// publishAged publishes an event through the bus, as the daemon does, and
// then backdates its row to at.
func publishAged(t *testing.T, svc *Service, bus events.Bus, at time.Time) int64 {
	t.Helper()
	if err := bus.Publish(context.Background(), events.Event{Scope: events.ScopeSession, SessionID: "s1", Kind: events.KindSessionStateChanged}); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := svc.Store.DB().QueryRow(`SELECT MAX(id) FROM events`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.DB().Exec(`UPDATE events SET at=? WHERE id=?`, at.UTC().Format(time.RFC3339Nano), id); err != nil {
		t.Fatal(err)
	}
	return id
}

func intPtr(n int) *int { return &n }

// Retention ships off: with no config, nothing is deleted.
func TestRunEventRetention_DisabledByDefault(t *testing.T) {
	svc, bus := retentionService(t, config.EventsRetentionConfig{})
	publishAged(t, svc, bus, time.Now().Add(-400*24*time.Hour))

	if n, err := svc.RunEventRetention(context.Background()); err != nil || n != 0 {
		t.Fatalf("deleted %d, %v; want 0 while disabled", n, err)
	}
	if evs, _ := svc.Store.EventsSince(0); len(evs) != 1 {
		t.Fatalf("events = %d; want the old one kept", len(evs))
	}
}

// Enabled, the sweep deletes everything older than the window across
// several bounded batches, and an SSE subscriber reconnecting from an event
// inside the window replays exactly what it would have before the sweep.
func TestRunEventRetention_DeletesOldInBatchesAndKeepsReplay(t *testing.T) {
	prevBatch, prevPause := eventsRetentionBatch, eventsRetentionBatchPause
	eventsRetentionBatch, eventsRetentionBatchPause = 2, 0
	t.Cleanup(func() { eventsRetentionBatch, eventsRetentionBatchPause = prevBatch, prevPause })

	svc, bus := retentionService(t, config.EventsRetentionConfig{Enabled: true, Days: intPtr(90)})
	now := time.Now()
	for i := 0; i < 5; i++ {
		publishAged(t, svc, bus, now.Add(-120*24*time.Hour+time.Duration(i)*time.Minute))
	}
	var kept []int64
	for _, age := range []time.Duration{60 * 24 * time.Hour, 24 * time.Hour, time.Minute} {
		kept = append(kept, publishAged(t, svc, bus, now.Add(-age)))
	}

	// What a client that last saw kept[0] gets on reconnect, before the sweep.
	replay := func() []int64 {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch, unsubscribe, err := bus.Subscribe(ctx, events.Filter{SinceSeq: kept[0]})
		if err != nil {
			t.Fatal(err)
		}
		defer unsubscribe()
		var got []int64
		for len(got) < 2 {
			select {
			case ev := <-ch:
				got = append(got, ev.Seq)
			case <-time.After(2 * time.Second):
				t.Fatalf("replay stalled after %v", got)
			}
		}
		return got
	}
	before := replay()

	n, err := svc.RunEventRetention(context.Background())
	if err != nil || n != 5 {
		t.Fatalf("deleted %d, %v; want the 5 rows older than 90 days", n, err)
	}
	evs, err := svc.Store.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != len(kept) {
		t.Fatalf("remaining = %d; want %d", len(evs), len(kept))
	}
	for i, ev := range evs {
		if ev.Seq != kept[i] {
			t.Fatalf("remaining[%d] = %d; want %d", i, ev.Seq, kept[i])
		}
	}
	after := replay()
	if len(after) != len(before) || after[0] != before[0] || after[1] != before[1] {
		t.Fatalf("replay after the sweep = %v; want %v, unchanged", after, before)
	}
	if n, err := svc.RunEventRetention(context.Background()); err != nil || n != 0 {
		t.Fatalf("second pass deleted %d, %v; want 0", n, err)
	}
}
