package workerhub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	re "github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// receiveActivity caches the last complete source batch. The observability
// consumer owns reduction. Connection fencing prevents a replaced socket from
// updating the new machine's view; scope claims from hook files are not trusted.
func (h *Hub) receiveActivity(id string, c *connection, batch *workerwire.Activity, now time.Time) error {
	if batch == nil || batch.ID == "" || len(batch.ID) > 128 || batch.ObservedAt.IsZero() || len(batch.Events) > 512 {
		return fmt.Errorf("invalid activity batch")
	}
	raw, err := json.Marshal(batch)
	if err != nil || len(raw) > 1<<20 {
		return fmt.Errorf("activity batch too large")
	}
	copyBatch := *batch
	copyBatch.Events = make([]re.Event, 0, len(batch.Events))
	seen := map[string]bool{}
	for _, e := range batch.Events {
		if e.Validate() != nil || e.SchemaVersion != re.SchemaVersion || e.Kind != re.KindSessionHeartbeat || e.Source.Channel != "plugin-hook" || e.Process.PID <= 0 {
			return fmt.Errorf("invalid activity event")
		}
		if e.Process.Provider != "claude" && e.Process.Provider != "codex" && e.Process.Provider != "agy" {
			return fmt.Errorf("invalid activity provider")
		}
		sid := e.Process.ProviderSessionID
		if sid == "" || len(sid) > 200 || strings.ContainsAny(sid, "\x00\r\n") || seen[e.Process.Provider+":"+sid] {
			return fmt.Errorf("invalid activity session")
		}
		seen[e.Process.Provider+":"+sid] = true
		// Rebuild instead of retaining producer-supplied actor/scope/cursors.
		copyBatch.Events = append(copyBatch.Events, re.Event{SchemaVersion: re.SchemaVersion, ID: id + ":" + e.ID, Kind: e.Kind, Time: e.Time, App: "tether-pulse", SessionID: id + ":" + e.Process.Provider + ":" + sid, Sequence: e.Sequence, Generation: e.Generation, Process: e.Process, Source: e.Source, Payload: append(json.RawMessage(nil), e.Payload...)})
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.connections[id] != c {
		return fmt.Errorf("activity connection replaced")
	}
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	c.activity, c.activityReceived = &copyBatch, now.UTC()
	return nil
}

// ActivityHTTP is a read-scoped relay of complete source observations. Old
// workers and disconnected workers are explicit, not an empty success scan.
func (h *Hub) ActivityHTTP(w http.ResponseWriter, r *http.Request) {
	workers, err := h.List(r.Context())
	if err != nil {
		http.Error(w, "worker inventory unavailable", 500)
		return
	}
	reports := make([]workerwire.ActivityReport, 0, len(workers))
	h.mu.RLock()
	for _, worker := range workers {
		report := workerwire.ActivityReport{WorkerID: worker.ID, Machine: worker.Label}
		if c := h.connections[worker.ID]; c != nil {
			c.pendingMu.Lock()
			report.Connected = true
			report.Activity, report.ReceivedAt = c.activity, c.activityReceived
			c.pendingMu.Unlock()
		}
		reports = append(reports, report)
	}
	h.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"reports": reports})
}
