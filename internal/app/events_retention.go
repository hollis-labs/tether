package app

import (
	"context"
	"log"
	"time"

	"github.com/hollis-labs/tether/internal/store"
)

var (
	// Bound each write transaction and yield between batches.
	eventsRetentionBatch      = 1000
	eventsRetentionBatchPause = 50 * time.Millisecond
)

func (s *Service) eventsRetention() time.Duration {
	if s.Catalog == nil {
		return 0
	}
	return s.Catalog.Global.Daemon.EventsRetention.Window()
}

// RetentionSweep is the post-delete integration hook. Counts and receipts
// describe only committed removals, including a partial sweep on error.
// This is not an archive-before-delete hook; no removed bodies are retained.
type RetentionSweep struct {
	Cutoff  time.Time              `json:"cutoff"`
	Removed map[string]int64       `json:"removed"`
	Batches []store.RetentionBatch `json:"batches"`
}

func (r RetentionSweep) Total() int64 {
	var n int64
	for _, count := range r.Removed {
		n += count
	}
	return n
}

// SweepEventRetention exposes a structured result for future consumers.
// The same catalog knob governs event histories, identity audit and terminal
// A2A tasks; disabled means no writes.
func (s *Service) SweepEventRetention(ctx context.Context) (RetentionSweep, error) {
	result := RetentionSweep{Removed: map[string]int64{}}
	window := s.eventsRetention()
	if window <= 0 || s.Store == nil {
		return result, nil
	}
	result.Cutoff = time.Now().Add(-window)
	for _, table := range []string{"events", "proxy_events", "ai_events", "identity_audit", "a2a_tasks"} {
		result.Removed[table] = 0
		for {
			batch, err := s.Store.DeleteEventHistoryBefore(ctx, table, result.Cutoff, eventsRetentionBatch)
			if err != nil {
				return result, err
			}
			result.Removed[table] += batch.Removed
			if batch.Removed > 0 {
				result.Batches = append(result.Batches, batch)
			}
			if batch.Removed < int64(eventsRetentionBatch) {
				break
			}
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(eventsRetentionBatchPause):
			}
		}
	}
	return result, nil
}

// RunEventRetention is the daemon's periodic-job seam. Structured output is
// emitted even for partial sweeps; the audit survives log rotation and expiry.
func (s *Service) RunEventRetention(ctx context.Context) (int64, error) {
	result, err := s.SweepEventRetention(ctx)
	if result.Total() > 0 {
		log.Printf("store: events retention removed=%v cutoff=%s", result.Removed, result.Cutoff.UTC().Format(time.RFC3339))
	}
	return result.Total(), err
}
