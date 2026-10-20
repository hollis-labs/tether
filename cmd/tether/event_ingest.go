package main

import (
	"errors"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/eventingest"
	"github.com/hollis-labs/tether/internal/events"
)

const (
	defaultEventIngestRate  = 200.0
	defaultEventIngestBurst = 1000
)

// newEventIngest builds the POST /events service over the daemon's bus and store.
func newEventIngest(svc *app.Service, s daemon.EventIngestSettings) (*eventingest.Service, error) {
	rate, burst := s.RatePerSecond, s.Burst
	if rate <= 0 {
		rate = defaultEventIngestRate
	}
	if burst <= 0 {
		burst = defaultEventIngestBurst
	}
	bus, ok := svc.Bus.(events.OutboxBus)
	if !ok {
		return nil, errors.New("event ingest: the daemon's event bus has no transactional outbox")
	}
	return eventingest.New(bus, svc.Store, eventingest.Config{
		AllowUnknownKinds: s.AllowUnknownKinds,
		RatePerSecond:     rate,
		Burst:             burst,
	})
}
