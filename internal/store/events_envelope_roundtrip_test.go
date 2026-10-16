package store

import (
	"context"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

func completeEnvelope() events.Event {
	e := fullEnvelope()
	e.At = time.Date(2003, 4, 5, 6, 7, 8, 123456789, time.FixedZone("producer", 3600))
	e.EventID = "evt_0123456789abcdef0123456789abcdef"
	env := &e.Envelope
	env.SchemaVersion, env.App, env.Subject = "1", "provider-app", "msg://session/s1"
	env.TurnID, env.ParentID = "turn-1", "evt_parent"
	zero := int64(0)
	env.RawOffset = &zero
	env.Process = events.Process{PID: 123, Provider: "provider", Runtime: "jsonrpc-stdio", ProviderSessionID: "upstream-1"}
	env.Source = events.Source{Channel: "native", Confidence: "exact"}
	env.Sequence, env.Generation = math.MaxUint64, math.MaxUint64
	env.Corr.Traceparent, env.Corr.InReplyTo = "00-trace-span-01", "evt_request"
	env.Actor.Role, env.Actor.OnBehalfOfChain = "worker", []string{"msg://agent/delegate", "msg://user/owner"}
	env.PayloadDigest = "withheld" // log-owned: discarded on write
	return e
}

func TestEnvelopeRoundTripEveryReadPath(t *testing.T) {
	db := openEventsStore(t)
	ctx := context.Background()
	input := completeEnvelope()
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := db.InsertEventTx(ctx, tx, input)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if !stored.At.Equal(input.At) || stored.Envelope.Epoch != 0 || stored.Envelope.PayloadDigest != "" {
		t.Fatalf("origin/log fields: %+v", stored)
	}
	if stored.Envelope.RecordedAt.Before(input.At) {
		t.Fatal("recorded time replaced by producer time")
	}
	queries := map[string]func() ([]events.Event, error){
		"session":     func() ([]events.Event, error) { return db.ListEventsBySession("s1", 10, 0) },
		"since":       func() ([]events.Event, error) { return db.EventsSince(0) },
		"page":        func() ([]events.Event, error) { return db.EventsPageContext(ctx, events.Filter{}, 0, stored.Seq, 10) },
		"recent":      func() ([]events.Event, error) { return db.ListRecentEvents(10) },
		"filtered":    func() ([]events.Event, error) { return db.QueryEvents(EventFilter{SessionID: "s1"}) },
		"outbox":      func() ([]events.Event, error) { return db.PendingEventsContext(ctx, 0, 10) },
		"environment": func() ([]events.Event, error) { w, err := db.EnvironmentEvents(ctx, 0, 10); return w.Events, err },
	}
	for name, query := range queries {
		t.Run(name, func(t *testing.T) {
			got, err := query()
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || !reflect.DeepEqual(got[0], stored) {
				t.Fatalf("round trip lost metadata:\ngot %+v\nwant %+v", got, stored)
			}
		})
	}
	// Replays of an already persisted producer ID return the original event,
	// including the origin time and envelope, not the retry's changed body.
	input.Kind, input.Envelope.App, input.At = "changed", "changed", time.Now()
	retry, err := db.InsertEventEnvelope(ctx, input)
	if err != nil || !reflect.DeepEqual(retry, stored) {
		t.Fatalf("duplicate changed persisted event: %+v, %v", retry, err)
	}
}

func TestEnvelopeOriginTimeDoesNotReorderReplay(t *testing.T) {
	db := openEventsStore(t)
	ctx := context.Background()
	newer := completeEnvelope()
	first, err := db.InsertEventEnvelope(ctx, newer)
	if err != nil {
		t.Fatal(err)
	}
	older := newer
	older.EventID = ""
	older.At = newer.At.Add(-time.Hour)
	second, err := db.InsertEventEnvelope(ctx, older)
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Seq != first.Seq || got[1].Seq != second.Seq || !got[1].At.Before(got[0].At) {
		t.Fatalf("origin time changed log ordering: %+v", got)
	}
}

func TestEnvironmentRequestRetainsEnvelope(t *testing.T) {
	db := openEventsStore(t)
	e := completeEnvelope()
	e.Kind = EnvironmentRequestKind
	e.PayloadJSON = `{"turn_id":"turn-1","request_id":"req-1","request_kind":"question","open":true,"source_sequence":1}`
	stored, err := db.InsertEventEnvelope(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.EventsSince(0)
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], stored) {
		t.Fatalf("request metadata lost: %+v, %v", got, err)
	}
}

func TestEnvelopeLegacyNullAndRetentionDigest(t *testing.T) {
	db := openEventsStore(t)
	if _, err := db.db.Exec(`INSERT INTO events(scope,at,kind) VALUES('daemon','2026-01-01T00:00:00Z','legacy')`); err != nil {
		t.Fatal(err)
	}
	got, err := db.EventsSince(0)
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0].Envelope, events.Envelope{}) {
		t.Fatalf("NULL legacy fields invented metadata: %+v, %v", got, err)
	}
	if _, err := db.db.Exec(`UPDATE events SET payload_digest='withheld' WHERE id=?`, got[0].Seq); err != nil {
		t.Fatal(err)
	}
	got, err = db.EventsSince(0)
	if err != nil || got[0].Envelope.PayloadDigest != "withheld" || got[0].PayloadJSON != "" {
		t.Fatalf("tombstone digest lost: %+v, %v", got, err)
	}
}

func TestEnvelopeBusReplayMatchesLive(t *testing.T) {
	db := openEventsStore(t)
	bus := newOutboxBus(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	live, unsub, err := bus.(events.LiveSubscriber).SubscribeLive(ctx, events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	if err := bus.Publish(ctx, completeEnvelope()); err != nil {
		t.Fatal(err)
	}
	published := collect(t, live, 1, 5*time.Second)[0]
	replay, stop, err := bus.Subscribe(ctx, events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	recovered := collect(t, replay, 1, 5*time.Second)[0]
	if !reflect.DeepEqual(published, recovered) {
		t.Fatalf("replay differs from live:\nlive %+v\nreplay %+v", published, recovered)
	}
}
