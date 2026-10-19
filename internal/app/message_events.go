package app

import (
	"context"
	"log"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

// StartMessageEvents drains durable mailbox observations into the same outbox
// used by the rest of the daemon. Only daemon startup activates this worker.
func (s *Service) StartMessageEvents(ctx context.Context) {
	bus, ok := s.Bus.(events.OutboxBus)
	if !ok || s.Store == nil {
		return
	}
	s.messageEventsMu.Lock()
	defer s.messageEventsMu.Unlock()
	if s.messageEventsStop != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.messageEventsStop = func() { cancel(); <-done }
	go func() {
		defer close(done)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			for range 8 {
				if ctx.Err() != nil {
					return
				}
				n, err := s.Store.DrainMessageEvents(ctx, bus)
				if err != nil {
					if ctx.Err() == nil {
						log.Printf("message events: drain: %v", err)
					}
					break
				}
				if n == 0 {
					break
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Service) stopMessageEvents() {
	s.messageEventsMu.Lock()
	stop := s.messageEventsStop
	s.messageEventsStop = nil
	s.messageEventsMu.Unlock()
	if stop != nil {
		stop()
	}
}
