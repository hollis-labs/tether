package api

// repair.go — T09 (messaging vNext, CW-20260906-0040): the authorized
// operator-repair surface the architecture calls for: "Implement...
// authorized retry/redrive with stable identity, bounded queues and
// observability." go-messaging's delivery.Store already has a Redrive
// primitive (delivery/types.go) -- before this file, nothing in Tether
// ever called it (confirmed by T09 design research: zero production
// call sites). This file is the first thing to expose it.
//
// Authorization: matches this repo's existing same-host, self-asserted
// trust model (ADR 0045) -- there is no cryptographic identity to check,
// so "authorized" here means "the caller supplied a URN-shaped
// authorized_by identity," recorded on the redrive for audit (go-
// messaging's own RedriveRequest.AuthorizedBy field), not a claim to
// verify. This is the same posture already applied throughout
// /messages/*, /groups/*, /broker/* -- a deliberate, disclosed choice,
// not a new gap introduced here.
//
// Idempotency: go-messaging's Redrive only succeeds on a currently
// dead-lettered delivery (returns ErrInvalidArgument otherwise) -- a
// second call after a successful redrive would therefore error even
// though "the delivery is retryable" (the caller's actual goal) already
// holds. handleMessageRedrive treats every non-terminal status
// (pending/leased/retry_scheduled) as "already achieved" and reports
// success with redriven=false, so a retried repair call is safe.
// Terminal outcomes (delivered/consumed/canceled) are reported as a
// distinct conflict, since redrive genuinely doesn't apply there and
// silently no-op'ing that case could mask a caller mistake.
//
// Frozen-fanout safety (acceptance #2's "cannot... retry frozen fanout
// against new actors"): this endpoint operates ONLY on a delivery_id --
// resolved from a stable message_id for the common 1:1 case, or taken
// directly when {id} IS already a delivery_id -- and never re-derives a
// recipient from current group membership. T04's fanOutGroupMessage
// already froze one RecipientDelivery row per member at the original
// fanout time (group_fanout.go); redriving delivery X always retries the
// SAME frozen recipient that row was created for, never a member who
// joined later. See repair_test.go's
// TestMessageRedrive_GroupFanout_TargetsFrozenRecipient for the concrete
// proof.
//
// Why {id} sometimes has to BE a delivery_id: a group-fanout message has
// N deliveries (one per member) but no corresponding row in Tether's own
// `messages` table at all -- fanOutGroupMessage enqueues straight into
// the delivery core via the registry's own GroupFanoutDeliveryStore seam
// (group_fanout.go), bypassing the generic message-store path entirely.
// DeliveryIDForMessage can't resolve a group message id because there is
// no such row to look up. An operator repairing ONE specific stuck
// recipient's fanout delivery therefore addresses it by that delivery's
// own id (obtained via GET .../trace or an operator's own inspection),
// not by any message id -- resolveDeliveryID tries the message-id
// lookup first (the common, unambiguous 1:1 case) and falls back to
// treating {id} as a literal delivery id only when that lookup finds
// nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/delivery"
)

// errNoDeliveryTracking is resolveDeliveryID's sentinel for "id matches
// neither a tracked message nor a known delivery" -- callers map it to
// 404.
var errNoDeliveryTracking = errors.New("api: no delivery tracking found for that id")

// resolveDeliveryID interprets id as a Tether message id first (the
// common case: DeliveryIDForMessage resolves it via messages.delivery_id).
// When that lookup finds nothing -- either because the row exists with
// no delivery_id (ok=false), or because no `messages` row with that id
// exists at all (DeliveryIDForMessage's own contract: it wraps
// messaging.ErrNotFound as a real error for that case, not just
// ok=false, since a legacy no-tracking row and a nonexistent id are
// different situations for ITS callers) -- id is tried as a literal
// delivery id directly against the delivery core. See the file doc
// comment for why group-fanout deliveries need this fallback.
func (s *Server) resolveDeliveryID(ctx context.Context, dts DeliveryTraceStore, id string) (string, error) {
	deliveryID, ok, err := dts.DeliveryIDForMessage(ctx, id)
	if err != nil && !errors.Is(err, messaging.ErrNotFound) {
		return "", err
	}
	if ok {
		return deliveryID, nil
	}
	if _, err := dts.DeliveryStore().GetDelivery(ctx, delivery.DeliveryID(id)); err == nil {
		return id, nil
	}
	return "", errNoDeliveryTracking
}

type messageRedriveRequest struct {
	// AuthorizedBy is a URN-shaped caller identity recorded on the
	// redrive for audit -- self-asserted per ADR 0045, not verified.
	AuthorizedBy       string `json:"authorized_by"`
	NewDeadlineSeconds int    `json:"new_deadline_seconds,omitempty"`
}

type messageRedriveResponse struct {
	MessageID  string `json:"message_id"`
	DeliveryID string `json:"delivery_id"`
	Status     string `json:"status"`
	// Redriven is true only when THIS call actually transitioned the
	// delivery out of dead_lettered. False + 200 means the delivery was
	// already in a non-terminal (retryable/in-flight) state -- the
	// idempotent no-op case.
	Redriven bool `json:"redriven"`
}

// handleMessageRedrive services POST /messages/{id}/redrive.
func (s *Server) handleMessageRedrive(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	if s.DeliveryRepair == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "delivery repair not configured")
		return
	}
	var req messageRedriveRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
			return
		}
	}
	if req.AuthorizedBy == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "authorized_by is required")
		return
	}
	if _, err := messaging.ParseURN(req.AuthorizedBy); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "authorized_by must be a valid URN: "+err.Error())
		return
	}

	ds := s.DeliveryRepair.DeliveryStore()
	deliveryID, err := s.resolveDeliveryID(r.Context(), s.DeliveryRepair, id)
	if err != nil {
		if errors.Is(err, errNoDeliveryTracking) {
			writeError(w, http.StatusNotFound, CodeNotFound, "no delivery tracking found for that id")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}

	before, err := ds.GetDelivery(r.Context(), delivery.DeliveryID(deliveryID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}

	if before.Status != delivery.DeliveryDeadLettered {
		if isTerminalDeliveryStatus(before.Status) {
			writeError(w, http.StatusConflict, CodeConflict,
				"delivery is already terminal ("+string(before.Status)+"); redrive does not apply")
			return
		}
		// Idempotent no-op: already pending/leased/retry_scheduled --
		// the caller's actual goal (this delivery is retryable) already
		// holds, so this is a success, not an error.
		writeJSON(w, http.StatusOK, messageRedriveResponse{
			MessageID: id, DeliveryID: deliveryID, Status: string(before.Status), Redriven: false,
		})
		return
	}

	var newDeadline time.Time
	if req.NewDeadlineSeconds > 0 {
		newDeadline = time.Now().UTC().Add(time.Duration(req.NewDeadlineSeconds) * time.Second)
	}
	after, err := ds.Redrive(r.Context(), delivery.RedriveRequest{
		DeliveryID:    delivery.DeliveryID(deliveryID),
		AuthorizedBy:  req.AuthorizedBy,
		NewDeadlineAt: newDeadline,
	})
	if err != nil {
		if errors.Is(err, delivery.ErrInvalidArgument) {
			// Lost a race with a concurrent redrive/claim between the
			// GetDelivery above and this call -- report the current
			// state honestly rather than a generic error.
			writeError(w, http.StatusConflict, CodeConflict, "delivery is no longer dead-lettered: "+err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, messageRedriveResponse{
		MessageID: id, DeliveryID: deliveryID, Status: string(after.Status), Redriven: true,
	})
}

func isTerminalDeliveryStatus(s delivery.DeliveryStatus) bool {
	switch s {
	case delivery.DeliveryDelivered, delivery.DeliveryCanceled:
		return true
	default:
		return false
	}
}
