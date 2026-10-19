package events_test

import (
	"context"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/eventstamp"
	"github.com/hollis-labs/tether/internal/store"
)

func TestPipelinePublishTxParityCommitAndRollback(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, err := events.NewPipeline(events.Enforce)
	if err != nil {
		t.Fatal(err)
	}
	stamp := eventstamp.New()
	stamp.SetProcess(eventstamp.Process{EnvironmentID: "fixture-env", Authority: "hub", Host: "fixture-host"})
	var databaseStageCalls atomic.Int64
	unsafe := events.StageFunc(func(ctx context.Context, _ *events.Event) error {
		databaseStageCalls.Add(1)
		var n int
		return db.DB().QueryRowContext(ctx, "SELECT 1").Scan(&n)
	})
	bus := events.NewBus(events.BusOptions{Persister: db, Pipeline: p, Stages: []events.Stage{stamp, unsafe}}).(events.OutboxBus)
	input := events.Event{Scope: events.ScopeSession, SessionID: "s1", Kind: events.KindSessionUsage, PayloadJSON: `{"input_tokens":1,"model":"synthetic@example.com"}`}
	if err := bus.Publish(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	tx, err := db.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5e9)
	defer cancel()
	transactional, err := bus.PublishTx(ctx, tx, input)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if databaseStageCalls.Load() != 1 {
		t.Fatal("non-TxSafe database stage ran inside transaction")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, err := db.EventsSince(0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if !reflect.DeepEqual(rows[0].Envelope.Class, transactional.Envelope.Class) || rows[0].PayloadJSON != transactional.PayloadJSON || rows[0].Envelope.Dims != transactional.Envelope.Dims || !transactional.Envelope.Class.Sanitized {
		t.Fatal("PublishTx bypassed shared chain")
	}
	tx, err = db.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bus.PublishTx(ctx, tx, input); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	rows, err = db.EventsSince(0)
	if err != nil || len(rows) != 2 {
		t.Fatal("rollback leaked event")
	}
}
