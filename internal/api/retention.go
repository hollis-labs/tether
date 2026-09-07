package api

// retention.go — T09 (messaging vNext, CW-20260906-0040): the HTTP
// surface over internal/store/retention.go's explicit, manual-only
// retention/purge mechanism. This file is thin plumbing over that
// store-layer policy -- the same split trace.go/repair.go already use
// for the DeliveryTraceStore seam.
//
// GET /messages/retention/candidates never mutates anything -- it exists
// so an operator previews what a purge run would affect before calling
// POST /messages/{id}/purge one message at a time. There is no bulk
// purge endpoint and nothing anywhere calls either of these
// automatically: purging is exclusively a one-message-at-a-time,
// explicitly authorized operator action, matching this sprint's
// manual-only launch policy and T09 acceptance #3's "no silent
// deletion."
//
// authorized_by on purge is recorded the same way repair.go's redrive
// records it -- a URN-shaped, self-asserted caller identity (ADR 0045) --
// but since purging has no library-owned schema to persist it in (unlike
// redrive's RedriveRequest.AuthorizedBy, which go-messaging itself
// stores), it is logged rather than persisted: a best-effort audit trail
// consistent with this package's existing precedent for the same
// constraint (delivery_store.go's Send() logs an orphaned-delivery id
// "for manual/T09 operator cleanup" rather than adding a new column for
// it).

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	messaging "github.com/hollis-labs/go-messaging"

	"github.com/hollis-labs/tether/internal/store"
)

// RetentionStore is the narrow seam GET /messages/retention/candidates
// and POST /messages/{id}/purge depend on. *store.Store satisfies it
// directly.
type RetentionStore interface {
	ListRetentionCandidates(ctx context.Context, olderThan time.Time) ([]store.RetentionCandidate, error)
	PurgeMessageBody(ctx context.Context, messageID string) (bool, error)
}

// registerRetentionRoutes mounts GET /messages/retention/candidates as an
// exact path, registered (like /messages/notify, /messages/inbox, ...)
// before the /messages/ prefix catch-all so ServeMux's longest-match
// picks it over handleMessagesItem's id-based dispatch. Only attached
// when Server.Retention is non-nil, matching every other optional
// dependency in this package.
func (s *Server) registerRetentionRoutes(mux *http.ServeMux) {
	if s.Retention == nil {
		return
	}
	mux.HandleFunc("/messages/retention/candidates", s.handleRetentionCandidates)
}

// retentionCandidateDTO is the wire shape for one ListRetentionCandidates
// row.
type retentionCandidateDTO struct {
	MessageID   string `json:"message_id"`
	CreatedAt   string `json:"created_at"`
	HasDelivery bool   `json:"has_delivery"`
	Status      string `json:"status,omitempty"`
	Eligible    bool   `json:"eligible"`
}

// defaultRetentionCandidateHours is the fallback lookback window (30
// days) when the caller doesn't specify older_than_hours.
const defaultRetentionCandidateHours = 24 * 30

// handleRetentionCandidates services GET /messages/retention/candidates.
// Read-only preview.
func (s *Server) handleRetentionCandidates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	if s.Retention == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "retention not configured")
		return
	}
	hours := defaultRetentionCandidateHours
	if v := r.URL.Query().Get("older_than_hours"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "older_than_hours must be a positive integer")
			return
		}
		hours = n
	}
	cutoff := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
	candidates, err := s.Retention.ListRetentionCandidates(r.Context(), cutoff)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	out := make([]retentionCandidateDTO, len(candidates))
	for i, c := range candidates {
		out[i] = retentionCandidateDTO{
			MessageID:   c.MessageID,
			CreatedAt:   formatTraceTime(c.CreatedAt),
			HasDelivery: c.HasDelivery,
			Status:      c.Status,
			Eligible:    c.Eligible,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidates": out})
}

type messagePurgeRequest struct {
	// AuthorizedBy is a URN-shaped caller identity, logged (not
	// persisted -- see file doc comment) for audit -- self-asserted per
	// ADR 0045, not verified.
	AuthorizedBy string `json:"authorized_by"`
}

type messagePurgeResponse struct {
	MessageID string `json:"message_id"`
	Purged    bool   `json:"purged"`
}

// handleMessagePurge services POST /messages/{id}/purge. Refuses with
// 409 when the message has a pending delivery obligation (see
// internal/store/retention.go for exactly what qualifies) -- never a
// silent no-op that could be mistaken for success.
func (s *Server) handleMessagePurge(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	if s.Retention == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "retention not configured")
		return
	}
	var req messagePurgeRequest
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

	purged, err := s.Retention.PurgeMessageBody(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrPendingObligation) {
			writeError(w, http.StatusConflict, CodeConflict, "message has a pending delivery obligation; refusing to purge")
			return
		}
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, CodeNotFound, "message not found")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	// %q (not %s) escapes any control characters in the two caller-
	// supplied strings (e.g. a newline forging a fake log line) before
	// they reach the log sink -- a real mitigation gosec's G706 taint
	// tracker doesn't credit format-verb-level sanitization for, hence
	// the explicit suppression below. This is the same same-host,
	// self-asserted trust model as everywhere else on this surface (ADR
	// 0045): id and authorized_by are logged for a local operator's own
	// audit trail, not fed to a security-sensitive log parser.
	log.Printf("api: message %q body purge authorized_by=%q purged=%v", id, req.AuthorizedBy, purged) //nolint:gosec // G706: escaped via %q; local operator audit log, not a security-parsed sink
	writeJSON(w, http.StatusOK, messagePurgeResponse{MessageID: id, Purged: purged})
}
