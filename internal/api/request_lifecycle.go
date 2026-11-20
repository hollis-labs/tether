package api

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/envelope"
	"github.com/hollis-labs/tether/internal/requestflow"
)

func (s *Server) submitRequest(ctx context.Context, e *envelope.Envelope, from, to messaging.Address, meta map[string]string) (requestflow.Request, error) {
	if meta["tether.message_id"] == "" {
		return requestflow.Request{}, fmt.Errorf("tracked requests require an idempotency key")
	}
	// The sender and principal-scoped key together identify a retry, including
	// when the first HTTP response was lost before any mail was delivered.
	online := to.URN() == identity.OperatorID
	if s.Service != nil && !online {
		sid, err := s.resolveNotifySession(ctx, "", to)
		online = err == nil && sid != ""
	}
	r, err := s.Requests.Submit(ctx, requestflow.Submit{
		Key: from.URN() + ":" + meta["tether.message_id"], TaskID: e.TaskID,
		RequesterURN: s.canonicalSeat(ctx, s.canonicalAlias(ctx, from.URN())), RecipientURN: to.URN(), Subject: e.Subject,
		Body: e.Body, Deliverable: e.Deliverable, RecipientOnline: online,
	})
	if err != nil {
		return r, err
	}
	if r.TaskID == "" {
		return r, fmt.Errorf("torque returned no request task id")
	}
	e.TaskID = r.TaskID
	meta["task_id"] = r.TaskID
	return r, nil
}

func (s *Server) settleRequest(ctx context.Context, e *envelope.Envelope, parent *envelope.Parent, from, recipient string, apply bool) error {
	if parent == nil || parent.Envelope == nil || parent.Envelope.Kind != envelope.KindRequest || parent.Envelope.TaskID == "" || e.Status == "" {
		return nil
	}
	pe := parent.Envelope
	if len(pe.To) != 1 {
		return fmt.Errorf("request must have exactly one recipient")
	}
	if recipient == "" || !s.actsFor(ctx, from, recipient) {
		return fmt.Errorf("only the request recipient may settle it")
	}
	e.TaskID = pe.TaskID
	if !apply {
		return nil
	}
	return s.Requests.Transition(ctx, pe.TaskID, requestflow.Transition{ActorURN: recipient, State: string(e.Status), Summary: e.Body, Evidence: e.Evidence})
}

func (s *Server) requestSent(ctx context.Context, r requestflow.Request, messageID string) error {
	if err := s.Requests.AttachMessage(ctx, r.TaskID, messageID); err != nil {
		return err
	}
	if !r.DueAckAt.After(time.Now()) && r.State == "submitted" && r.AlertedAt == nil {
		// Mail failure leaves the durable due row for the daemon's next sweep;
		// it does not pretend the original request was rejected.
		alerts := requestflow.Alerts{Backend: s.Requests, Sender: s.MessageStore}
		if err := alerts.Deliver(ctx, r); err != nil {
			slog.Warn("immediate request alert deferred", "task_id", r.TaskID, "error", err)
		}
	}
	return nil
}
