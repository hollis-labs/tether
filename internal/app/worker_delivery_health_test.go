package app

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerhub"
)

// CW-20261114-0104: a worker that stops answering commands is visible as a worker.delivery_degraded event,
// scoped to the worker, with the run of timeouts, the ingest backlog and what the hub did.
func TestWorkerDeliveryDegradedEvent(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := &Service{Store: db, Bus: events.NewBus(events.BusOptions{Persister: db})}
	s.publishWorkerDegraded("worker_x", workerhub.DeliveryDegraded{ConsecutiveTimeouts: 6, IngestBacklog: 4096, DroppedFrames: 12, Action: "reconnect"})
	var raw, worker string
	if err := db.DB().QueryRow(`SELECT payload_json, COALESCE(scope_worker_id,'') FROM events WHERE kind=?`, KindWorkerDeliveryDegraded).Scan(&raw, &worker); err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	_ = json.Unmarshal([]byte(raw), &p)
	if p["worker_id"] != "worker_x" || p["consecutive_timeouts"] != float64(6) || p["ingest_backlog"] != float64(4096) || p["action"] != "reconnect" {
		t.Fatalf("payload = %s", raw)
	}
	if worker != "worker_x" {
		t.Fatalf("scope_worker_id = %q", worker)
	}
}
