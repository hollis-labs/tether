package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
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

// The daemon's own service wires the health tracker, the stamp stage and the
// row stage onto its bus: a running session produces a health transition whose
// row carries the session's scope.
func TestNewServiceWiresTheStampStagesAndTheHealthTracker(t *testing.T) {
	svc, err := New(filepath.Join(t.TempDir(), "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if svc.EventStamp == nil {
		t.Fatal("no stamp stage on the service")
	}
	row := store.SessionRow{ID: "s-wired", LaunchID: "l", ProjectID: "PRJ-W", LogicalAgentID: "la-w", ProviderID: "claude", WorkerID: "w-9", State: "running"}
	if err = svc.Store.CreateSession(row, nil); err != nil {
		t.Fatal(err)
	}
	publishSessionEvent(svc.Bus, "s-wired", "la-w", events.KindSessionStateChanged, sessionStateChangedPayload{From: "launching", To: "running"})
	var payload, worker, project, schema sql.NullString
	err = svc.Store.DB().QueryRow(`SELECT payload_json, scope_worker_id, scope_project_id, payload_schema FROM events WHERE kind=? AND session_id='s-wired'`, events.KindSessionHealthChanged).
		Scan(&payload, &worker, &project, &schema)
	if err != nil {
		t.Fatalf("no health transition for a running session: %v", err)
	}
	if worker.String != "w-9" || project.String != "PRJ-W" || schema.String != "session_health_changed@1" || !strings.Contains(payload.String, `"to":"healthy"`) || !strings.Contains(payload.String, "session_running") {
		t.Fatalf("health event %s worker=%q project=%q schema=%q", payload.String, worker.String, project.String, schema.String)
	}
}
