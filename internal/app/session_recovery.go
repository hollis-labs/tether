package app

import (
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

// MarkSessionDetached records loss of the daemon connection to a live child.
func (s *Service) MarkSessionDetached(id, reason string) error {
	change, err := s.Store.MarkSessionDetached(id, reason)
	if err != nil {
		return err
	}
	s.publishSessionStateChange(change)
	return nil
}

// MarkSessionOrphaned records a vanished shim/child and revokes its authority.
func (s *Service) MarkSessionOrphaned(id, reason string) error {
	change, err := s.Store.MarkSessionOrphaned(id, reason)
	if err != nil {
		return err
	}
	s.publishSessionStateChange(change)
	return nil
}

func (s *Service) publishSessionStateChange(change store.SessionStateChange) {
	if change.From == change.To {
		return
	}
	publishSessionEvent(s.Bus, change.SessionID, change.LogicalAgentID, events.KindSessionStateChanged,
		sessionStateChangedPayload{From: change.From, To: change.To, Reason: change.Reason})
}
