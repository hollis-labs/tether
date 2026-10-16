package app

import (
	"context"

	"github.com/hollis-labs/tether/internal/events"
)

// StartEventsOutbox starts the events outbox drainer (CW-20261016-0136, X1a):
// it first publishes the event rows a previous process committed on a
// transaction but never published, then publishes new ones after their commit
// (store.EventsCommitted) and on a periodic sweep. Only the daemon calls it.
func (s *Service) StartEventsOutbox(ctx context.Context) {
	ob, ok := s.Bus.(events.OutboxBus)
	if !ok {
		return
	}
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	if s.outboxStop != nil {
		return
	}
	s.outboxStop = ob.StartOutbox(ctx)
}

func (s *Service) stopEventsOutbox() {
	s.outboxMu.Lock()
	stop := s.outboxStop
	s.outboxStop = nil
	s.outboxMu.Unlock()
	if stop != nil {
		stop()
	}
}
