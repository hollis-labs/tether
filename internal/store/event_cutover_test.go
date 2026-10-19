package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

type legacyEventPersister struct{ db *Store }

func (p legacyEventPersister) InsertEvent(scope events.Scope, s, k, raw string) (int64, time.Time, error) {
	return p.db.InsertEvent(scope, s, k, raw)
}
func (p legacyEventPersister) EventsSince(n int64) ([]events.Event, error) {
	return p.db.EventsSince(n)
}

type contextEventPersister struct{ legacyEventPersister }

func (p contextEventPersister) InsertEventContext(ctx context.Context, scope events.Scope, s, k, raw string) (int64, time.Time, error) {
	return p.db.InsertEventContext(ctx, scope, s, k, raw)
}
func (p contextEventPersister) EventsSinceContext(ctx context.Context, n int64) ([]events.Event, error) {
	return p.db.EventsSinceContext(ctx, n)
}

type idEventPersister struct{ legacyEventPersister }

func (p idEventPersister) InsertEventIDContext(ctx context.Context, scope events.Scope, s, k, raw string) (int64, time.Time, string, error) {
	return p.db.InsertEventIDContext(ctx, scope, s, k, raw)
}

func TestEventCutoverEveryBusPersistBranchMintsAndGuardRefusesNull(t *testing.T) {
	db := openEventsStore(t)
	ctx := context.Background()
	if _, err := db.SeedEventIdentity(ctx, frozenTestEnvironment); err != nil {
		t.Fatal(err)
	}
	for _, p := range []events.Persister{legacyEventPersister{db}, contextEventPersister{legacyEventPersister{db}}, idEventPersister{legacyEventPersister{db}}, db} {
		if err := events.NewBus(events.BusOptions{Persister: p}).Publish(ctx, events.Event{Scope: events.ScopeDaemon, Kind: "new", PayloadJSON: "{}"}); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.NewBus(events.BusOptions{Persister: db}).(events.OutboxBus).PublishTx(ctx, tx, events.Event{Scope: events.ScopeDaemon, Kind: "tx", PayloadJSON: "{}"}); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	es, err := db.EventsSince(0)
	if err != nil || len(es) != 5 {
		t.Fatal(es, err)
	}
	seen := map[string]bool{}
	for _, e := range es {
		if !strings.HasPrefix(e.EventID, "evt_") || seen[e.EventID] {
			t.Fatal("unminted/reused id", e)
		}
		seen[e.EventID] = true
	}
	for _, id := range []any{nil, ""} {
		if _, err := db.DB().Exec(`INSERT INTO events(scope,kind,at,event_id) VALUES('daemon','bad','2026-01-01T00:00:00Z',?)`, id); err == nil {
			t.Fatal("identity-less insert accepted")
		}
	}
}

func TestEventCutoverRetentionDeletionDuringBackfill(t *testing.T) {
	db := openEventsStore(t)
	ctx := context.Background()
	insertLegacyIDFixture(t, db, 5)
	if _, err := db.SeedEventIdentity(ctx, frozenTestEnvironment); err != nil {
		t.Fatal(err)
	}
	if _, err := db.BackfillEventIDs(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`DELETE FROM events WHERE id IN (2,3,4)`); err != nil {
		t.Fatal(err)
	}
	p, err := db.BackfillEventIDs(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.Remaining != 0 {
		p, err = db.BackfillEventIDs(ctx, 1)
	}
	if err != nil || p.Remaining != 0 || p.Backfilled != 2 || p.Dropped != 0 {
		t.Fatal(p, err)
	}
	es, err := db.EventsSince(0)
	if err != nil || len(es) != 2 || es[0].EventID != frozenTestEnvironment+":1" || es[1].EventID != frozenTestEnvironment+":5" {
		t.Fatal("retention invented IDs", es, err)
	}
}
