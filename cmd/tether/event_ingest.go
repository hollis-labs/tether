package main

import (
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/eventingest"
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
	return eventingest.New(svc.Bus, svc.Store, eventingest.Config{
		AllowUnknownKinds: s.AllowUnknownKinds,
		RatePerSecond:     rate,
		Burst:             burst,
	})
}
