package app

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/workerhub"
)

// KindWorkerDeliveryDegraded is published when commands to a connected worker (turns, mail wakes, stops)
// keep going unanswered (CW-20261114-0104): action "reported" after workerhub.DegradedAfter timeouts in a
// row, "reconnect" when the hub closes the connection after workerhub.ReconnectAfter so the worker
// reconnects and resumes.
const KindWorkerDeliveryDegraded = "worker.delivery_degraded"

// publishWorkerDegraded is the hub's Degraded callback.
func (s *Service) publishWorkerDegraded(workerID string, d workerhub.DeliveryDegraded) {
	if s.Bus == nil {
		return
	}
	raw, _ := json.Marshal(map[string]any{"worker_id": workerID, "consecutive_timeouts": d.ConsecutiveTimeouts,
		"ingest_backlog": d.IngestBacklog, "dropped_frames": d.DroppedFrames, "action": d.Action})
	e := events.Event{Scope: events.ScopeDaemon, Kind: KindWorkerDeliveryDegraded, At: time.Now().UTC(), PayloadJSON: string(raw)}
	e.Envelope.Dims.WorkerID = workerID
	e.Envelope.Class.Category = "state"
	e.Envelope.Class.Severity = "warning"
	if d.Action == "reconnect" {
		e.Envelope.Class.Severity = "error"
	}
	if err := s.Bus.Publish(context.Background(), e); err != nil {
		log.Printf("app: publish %s for worker %s: %v", KindWorkerDeliveryDegraded, workerID, err)
	}
}
