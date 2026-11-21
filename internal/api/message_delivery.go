package api

import (
	"net/http"

	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

// A permission to inspect or send as somebody else is not evidence that the
// recipient process received mail. Check owned addresses without scope grants.
func (s *Server) isDeliveryRecipient(r *http.Request, to string) bool {
	p, ok := verifiedPrincipal(r)
	if !ok {
		return false
	}
	p.Scopes = nil
	if s.claimHolds(r.Context(), p, to) {
		return true
	}
	// Bound agent JWTs carry the session address, not an actor address claim.
	// Resolve their actor from daemon-owned session/binding records, never from
	// as_session or an administrator's permission to impersonate a recipient.
	if s.Service == nil || p.SessionID == "" || (p.Kind != "session" && !identity.IsBoundAgentToken(p)) {
		return false
	}
	addr, err := messaging.ParseURN(to)
	if err != nil {
		return false
	}
	if addr.Kind == messaging.KindSession {
		return addr.ID == p.SessionID
	}
	if addr.Kind != messaging.KindAgent {
		return false
	}
	if (inboxSessionLookup{s.Service}).BoundToActor(p.SessionID, addr.ID) {
		return true
	}
	row, err := s.Service.GetSession(p.SessionID)
	return err == nil && row != nil && row.LogicalAgentID != "" && s.sameAddress(r.Context(), to, registry.LogicalAgentBindingTarget(row.LogicalAgentID))
}

func (s *Server) markMessageDelivered(r *http.Request, id string, to messaging.Address, path string) error {
	if !s.isDeliveryRecipient(r, to.URN()) {
		return nil
	}
	marker, ok := s.MessageStore.(store.DeliveryMarker)
	if !ok {
		return nil
	}
	p, _ := verifiedPrincipal(r)
	return marker.MarkDelivered(r.Context(), id, to, path, p.SessionID)
}

// The declaration is accepted only by handleMessagesWatch after checking the
// authenticated operator credential; it never changes list/get/inbox semantics.
func (s *Server) markWatchMessageDelivered(r *http.Request, id string, to messaging.Address, path string) error {
	p, _ := verifiedPrincipal(r)
	if path == "operator-declared-watch" && identity.IsOperatorCredential(p) {
		if marker, ok := s.MessageStore.(store.DeliveryMarker); ok {
			return marker.MarkDelivered(r.Context(), id, to, path, p.SessionID)
		}
		return nil
	}
	return s.markMessageDelivered(r, id, to, path)
}
