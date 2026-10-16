package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/eventstamp"
	"github.com/hollis-labs/tether/internal/store"
)

// A lifecycle event for a session on a worker is stored with the session's
// row-derived scope dimensions and carries its owned fields in the payload.
func TestSessionStateChangedCarriesSessionFieldsAndRowDimensions(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	row := store.SessionRow{ID: "s-worker", LaunchID: "tether-claude", ProjectID: "PRJ-1", LogicalAgentID: "la-1", ProviderID: "claude", ProviderKind: "cli", WorkerID: "worker-1", State: "running", Intent: "resume",
		ParentSessionID: sql.NullString{String: "s-parent", Valid: true}, WorkstreamID: sql.NullString{String: "ws-1", Valid: true}}
	if err = db.CreateSession(row, nil); err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus(events.BusOptions{Persister: db, Stages: []events.Stage{eventstamp.New(), eventstamp.NewRows(stampSessions{db}, 0)}})
	sink := &eventSinkAdapter{db: db, bus: bus}
	sink.Emit(context.Background(), agentsessions.LifecycleEvent{SessionID: "s-worker", From: agentsessions.StateRunning, To: agentsessions.StateDone})

	var payload, worker, project, workstream, launch, logical sql.NullString
	err = db.DB().QueryRow(`SELECT payload_json, scope_worker_id, scope_project_id, scope_workstream_id, scope_launch_id, scope_logical_agent_id FROM events WHERE kind=?`, events.KindSessionStateChanged).
		Scan(&payload, &worker, &project, &workstream, &launch, &logical)
	if err != nil {
		t.Fatal(err)
	}
	if worker.String != "worker-1" || project.String != "PRJ-1" || workstream.String != "ws-1" || launch.String != "tether-claude" || logical.String != "la-1" {
		t.Fatalf("stored dims: worker=%q project=%q workstream=%q launch=%q logical=%q", worker.String, project.String, workstream.String, launch.String, logical.String)
	}
	var got sessionStateChangedPayload
	if err = json.Unmarshal([]byte(payload.String), &got); err != nil {
		t.Fatal(err)
	}
	want := sessionFields{ProviderID: "claude", ProviderKind: "cli", WorkerID: "worker-1", ParentSessionID: "s-parent", Intent: "resume"}
	if got.Session == nil || *got.Session != want || got.To != "completed" {
		t.Fatalf("payload %s", payload.String)
	}
}

// A session row that is gone still publishes the transition.
func TestSessionStateChangedWithoutARowStillPublishes(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	bus := events.NewBus(events.BusOptions{Persister: db, Stages: []events.Stage{eventstamp.NewRows(stampSessions{db}, 0)}})
	sink := &eventSinkAdapter{db: db, bus: bus}
	sink.Emit(context.Background(), agentsessions.LifecycleEvent{SessionID: "ghost", From: agentsessions.StateRunning, To: agentsessions.StateDone})
	var payload string
	var worker sql.NullString
	if err = db.DB().QueryRow(`SELECT payload_json, scope_worker_id FROM events WHERE kind=?`, events.KindSessionStateChanged).Scan(&payload, &worker); err != nil {
		t.Fatal(err)
	}
	var got sessionStateChangedPayload
	if err = json.Unmarshal([]byte(payload), &got); err != nil || got.Session != nil || worker.Valid {
		t.Fatalf("payload %s worker %v err %v", payload, worker, err)
	}
}
