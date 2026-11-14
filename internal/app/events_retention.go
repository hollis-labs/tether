package app

import (
	"context"
	"log"
	"time"

	"github.com/hollis-labs/tether/internal/eventretention"
	"github.com/hollis-labs/tether/internal/store"
)

var (
	// Bound each write transaction and yield between batches.
	eventsRetentionBatch      = 1000
	eventsRetentionBatchPause = 50 * time.Millisecond
)

func (s *Service) eventsRetention() time.Duration {
	if s.cat() == nil {
		return 0
	}
	return s.cat().Global.Daemon.EventsRetention.Window()
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
	// R-debug rows (raw session.output) go first, on their own shorter window
	// (CW-20261114-0019). Unclassified rows, including session.output written
	// before admission, stay on the main window.
	if debug := s.eventsDebugRetention(); debug > 0 && debug < window {
		if err := s.sweepRetentionTable(ctx, &result, "events:debug", time.Now().Add(-debug)); err != nil {
			return result, err
		}
	}
	for _, table := range []string{"events", "event_ingest_digests", "turn_inputs", "proxy_events", "ai_events", "identity_audit", "a2a_tasks"} {
		if err := s.sweepRetentionTable(ctx, &result, table, result.Cutoff); err != nil {
			return result, err
		}
	}
	return result, nil
}

// sweepRetentionTable deletes table's rows older than cutoff in bounded batches.
func (s *Service) sweepRetentionTable(ctx context.Context, result *RetentionSweep, table string, cutoff time.Time) error {
	result.Removed[table] = 0
	for {
		batch, err := s.Store.DeleteEventHistoryBefore(ctx, table, cutoff, eventsRetentionBatch)
		if err != nil {
			return err
		}
		result.Removed[table] += batch.Removed
		if batch.Removed > 0 {
			result.Batches = append(result.Batches, batch)
		}
		if batch.Removed < int64(eventsRetentionBatch) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(eventsRetentionBatchPause):
		}
	}
}

func (s *Service) eventsDebugRetention() time.Duration {
	if s.cat() == nil {
		return 0
	}
	return s.cat().Global.Daemon.EventsRetention.DebugWindow()
}

// eventRetentionKinds are the kinds admitted to a retention class
// (CW-20261114-0019). Raw provider output is debug: it is about 98% of event
// rows, and its useful content already lands in session.turn_output and the
// session log. Every other kind stays unclassified (held on the main window).
var eventRetentionKinds = map[string]eventretention.Class{
	"session.output": eventretention.Debug,
}

// AdmitEventRetentionClasses installs eventRetentionKinds under the default
// retention policy, for rows written from now on. Existing rows keep their
// class (none); nothing is backfilled.
func (s *Service) AdmitEventRetentionClasses(ctx context.Context) error {
	if s.Store == nil {
		return nil
	}
	cfg, _, err := eventretention.Parse(nil)
	if err != nil {
		return err
	}
	return s.Store.SetEventRetentionAdmission(ctx, cfg, eventRetentionKinds)
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
