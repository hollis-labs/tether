package store_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/eventstamp"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/store"
)

func stagedStoreBus(t *testing.T, db *store.Store) events.OutboxBus {
	t.Helper()
	stamp := eventstamp.New()
	stamp.SetProcess(eventstamp.Process{EnvironmentID: "env", Host: "host"})
	bus := events.NewBus(events.BusOptions{Persister: db, Stages: []events.Stage{stamp}}).(events.OutboxBus)
	db.SetEventTxPublisher(bus.PublishTx)
	db.SetEventsCommittedHook(bus.KickOutbox)
	return bus
}

func TestStoreProducersUseBusStagesAndCallerTransaction(t *testing.T) {
	db := openRetentionDB(t)
	bus := stagedStoreBus(t, db)
	if err := db.CreateSession(store.SessionRow{ID: "s", State: "running", WorkerID: "worker", ProjectID: "project", LogicalAgentID: "logical", LaunchID: "launch", TaskID: sql.NullString{String: "task", Valid: true}}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// The known operator actor is independent of the session being observed.
	ctx = identity.WithPrincipal(ctx, identity.Principal{Verified: true, ID: "msg://user/local/operator", Kind: "user"})
	if err := db.RecordSessionActivity(ctx, "s", time.Now()); err != nil {
		t.Fatal(err)
	}
	if changed, err := db.RecordReaperOrphan(ctx, "s", "process_missing"); err != nil || !changed {
		t.Fatal(changed, err)
	}
	pending, err := db.PendingEventsContext(ctx, 0, 10)
	if err != nil || len(pending) != 2 {
		t.Fatal(pending, err)
	}
	for _, e := range pending {
		d, a := e.Envelope.Dims, e.Envelope.Actor
		if !strings.HasPrefix(e.EventID, "evt_") || e.LogicalAgentID != "logical" || d.LogicalAgentID != "logical" || d.WorkerID != "worker" || d.ProjectID != "project" || d.TaskID != "task" || d.LaunchID != "launch" || d.EnvironmentID != "env" || d.Host != "host" || a.URN != "msg://user/local/operator" || a.Verified != "token" || e.Envelope.Generation != 0 {
			t.Fatalf("unstamped or invented envelope %+v", e)
		}
	}
	// A later ordinary Publish drains committed rows first, preserving log order.
	if err := bus.Publish(ctx, events.Event{Scope: events.ScopeSession, SessionID: "s", Kind: "test.after"}); err != nil {
		t.Fatal(err)
	}
	pending, err = db.PendingEventsContext(ctx, 0, 10)
	if err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
}

func TestStoreProducerPublishFailureRollsBackBusinessChange(t *testing.T) {
	db := openRetentionDB(t)
	if err := db.CreateSession(store.SessionRow{ID: "s", State: "running"}, nil); err != nil {
		t.Fatal(err)
	}
	rejected := errors.New("publisher unavailable")
	db.SetEventTxPublisher(func(context.Context, events.Tx, events.Event) (events.Event, error) { return events.Event{}, rejected })
	if changed, err := db.RecordReaperOrphan(context.Background(), "s", "process_missing"); changed || !errors.Is(err, rejected) {
		t.Fatal(changed, err)
	}
	row, err := db.GetSession("s")
	if err != nil || row.State != "running" {
		t.Fatal("failed audit committed mutation", row, err)
	}
	es, err := db.EventsSince(0)
	if err != nil || len(es) != 0 {
		t.Fatal("failed tx left event", es, err)
	}
}

func TestRoutedAuditUsesBusStagesOnAttachmentTransaction(t *testing.T) {
	db, _, req := channelStage(t)
	stagedStoreBus(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := channels.New(db, nil).AttachExisting(ctx, req); err != nil {
		t.Fatal(err)
	}
	es, err := db.PendingEventsContext(ctx, 0, 10)
	if err != nil || len(es) != 1 {
		t.Fatal(es, err)
	}
	e := es[0]
	if !strings.HasPrefix(e.EventID, "evt_") || e.Kind != events.KindSessionTurnRouted || e.Envelope.Dims.EnvironmentID != "env" || e.Envelope.Dims.LaunchID != "ops-launch" || e.Envelope.Actor.URN != "" {
		t.Fatalf("audit skipped stages or invented actor: %+v", e)
	}
}
