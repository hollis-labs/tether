package daemon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
)

func (s *Server) recordIdentityObservation(ctx context.Context, o identity.Observation) error {
	if s.Identity == nil {
		return fmt.Errorf("identity store unavailable")
	}
	if err := s.Identity.RecordObservation(ctx, o); err != nil {
		return err
	}
	if s.Publisher == nil {
		return nil
	}
	body, err := json.Marshal(o)
	if err != nil {
		return fmt.Errorf("encode identity event: %w", err)
	}
	return s.Publisher.Publish(ctx, events.Event{
		At: o.At, Scope: events.ScopeDaemon, SessionID: o.SessionID,
		Kind: events.KindIdentityObserved, PayloadJSON: string(body),
	})
}
