package app

import (
	"context"
	"log"
	"time"
)

// Events retention sweep (CW-20260930-0008, decision D-50). The events table
// is the durable backbone behind GET /events, /events/stream replay and
// /sessions/{id}/events, and nothing ever deleted from it. The sweep deletes
// rows older than the window, by age only: a row-count cap was rejected
// because it could drop a long-running session's recent history at an
// arbitrary point. It is off unless daemon.events_retention.enabled is set
// (window: days, default 90); see config.EventsRetentionConfig.

var (
	// eventsRetentionBatch bounds one DELETE statement, so a large first
	// sweep releases the write lock between batches instead of holding it
	// for the whole backlog. A var so tests can shrink it.
	eventsRetentionBatch = 1000
	// eventsRetentionBatchPause separates batches, leaving room for the
	// daemon's own writes on the single shared connection.
	eventsRetentionBatchPause = 50 * time.Millisecond
)

// eventsRetention returns the configured window, or 0 when disabled.
func (s *Service) eventsRetention() time.Duration {
	if s.Catalog == nil {
		return 0
	}
	return s.Catalog.Global.Daemon.EventsRetention.Window()
}

// RunEventRetention runs one retention pass: it deletes every event older
// than the window, in bounded batches, and returns how many it deleted. The
// daemon calls it periodically; a no-op when retention is disabled.
func (s *Service) RunEventRetention(ctx context.Context) (int64, error) {
	window := s.eventsRetention()
	if window <= 0 || s.Store == nil {
		return 0, nil
	}
	cutoff := time.Now().Add(-window)
	var total int64
	for {
		n, err := s.Store.DeleteEventsBefore(ctx, cutoff, eventsRetentionBatch)
		total += n
		if err != nil {
			logEventsRetention(total, cutoff, window)
			return total, err
		}
		if n < int64(eventsRetentionBatch) {
			break
		}
		select {
		case <-ctx.Done():
			logEventsRetention(total, cutoff, window)
			return total, ctx.Err()
		case <-time.After(eventsRetentionBatchPause):
		}
	}
	logEventsRetention(total, cutoff, window)
	return total, nil
}

func logEventsRetention(deleted int64, cutoff time.Time, window time.Duration) {
	if deleted == 0 {
		return
	}
	log.Printf("store: events retention deleted %d event(s) older than %s (window %d days)",
		deleted, cutoff.UTC().Format(time.RFC3339), int(window/(24*time.Hour)))
}
