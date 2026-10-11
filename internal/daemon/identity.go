package daemon

import (
	"context"
	"fmt"
	"github.com/hollis-labs/tether/internal/identity"
)

func (s *Server) recordIdentityObservation(ctx context.Context, o identity.Observation) error {
	if s.Identity == nil {
		return fmt.Errorf("identity store unavailable")
	}
	s.identityAuditMu.Lock()
	if s.identityAudit == nil {
		s.identityAudit = identity.NewAuditQueue(256, s.Identity.RecordObservation)
	}
	queue := s.identityAudit
	s.identityAuditMu.Unlock()
	return queue.Enqueue(ctx, o)
}
func (s *Server) CloseIdentityAudit() {
	s.identityAuditMu.Lock()
	queue := s.identityAudit
	s.identityAuditMu.Unlock()
	if queue != nil {
		queue.Close()
	}
}

type IdentityHealth struct {
	Mode             identity.Mode `json:"mode"`
	OperatorDegraded bool          `json:"operator_degraded"`
	// Points lists per-enforcement-point overrides in effect (CW-20261010-0268).
	Points map[string]identity.Mode `json:"points,omitempty"`
	Audit  identity.AuditStats      `json:"audit"`
}

func (s *Server) identityHealth() *IdentityHealth {
	h := &IdentityHealth{Mode: s.Config.IdentityMode, OperatorDegraded: s.OperatorIdentityDegraded, Points: s.Config.IdentityPoints}
	s.identityAuditMu.Lock()
	queue := s.identityAudit
	s.identityAuditMu.Unlock()
	if queue != nil {
		h.Audit = queue.Stats()
	}
	return h
}
