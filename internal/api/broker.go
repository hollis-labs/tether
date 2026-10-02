package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/hollis-labs/tether/internal/broker"
)

// Shared with the messaging request route; the broker owns this legacy default.
const defaultRequestTimeout = broker.DefaultRequestTimeout

// BrokerService is the seam the broker handlers depend on. Bundles
// broker.Service's write path (events emitted for free) with the
// store's read path — callers supply an adapter that composes both
// behaviors. Keeping this as a single interface over two concerns
// avoids a Deps.Broker + Deps.BrokerReader split; the cmd layer
// already owns the composition and can implement both halves.
type BrokerService interface {
	CreateEnvelope(ctx context.Context, e broker.Envelope) error
	ReplyEnvelope(ctx context.Context, reply broker.Envelope) error
	GetEnvelope(id string) (*broker.Envelope, error)
	ListEnvelopesByRecipient(recipient string) ([]broker.Envelope, error)
	ListEnvelopesByWorkflow(workflowID, correlationID string) ([]broker.Envelope, error)
	// WaitForResponse blocks until a response envelope with the given
	// correlationID is delivered or ctx is canceled.
	WaitForResponse(ctx context.Context, correlationID string) (*broker.Envelope, error)
}

// EnvelopeCreateRequest is the JSON body for POST /broker/envelopes.
// Server-set fields (ID, CreatedAt) are omitted; every other field is
// optional and round-trips verbatim into broker.Envelope.
type EnvelopeCreateRequest broker.CreateRequest

// EnvelopeDTO is the wire shape of an envelope.
type EnvelopeDTO struct {
	ID            string `json:"id"`
	Sender        string `json:"sender,omitempty"`
	Recipient     string `json:"recipient,omitempty"`
	WorkflowID    string `json:"workflow_id,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
	MessageType   string `json:"message_type,omitempty"`
	Priority      int    `json:"priority,omitempty"`
	Payload       string `json:"payload,omitempty"`
	CreatedAt     string `json:"created_at"`
	DeliveredAt   string `json:"delivered_at,omitempty"`
	ConsumedAt    string `json:"consumed_at,omitempty"`
	AuditJSON     string `json:"audit_json,omitempty"`
}

type EnvelopeListResponse struct {
	Envelopes []EnvelopeDTO `json:"envelopes"`
}

func envelopeToDTO(e broker.Envelope) EnvelopeDTO {
	return EnvelopeDTO{
		ID:            e.ID,
		Sender:        e.Sender,
		Recipient:     e.Recipient,
		WorkflowID:    e.WorkflowID,
		CorrelationID: e.CorrelationID,
		MessageType:   e.MessageType,
		Priority:      e.Priority,
		Payload:       e.Payload,
		CreatedAt:     e.CreatedAt,
		DeliveredAt:   e.DeliveredAt,
		ConsumedAt:    e.ConsumedAt,
		AuditJSON:     e.AuditJSON,
	}
}

func (s *Server) registerBrokerRoutes(router *http.ServeMux) {
	if s.Broker == nil {
		return
	}
	router.HandleFunc("/broker/envelopes", s.handleEnvelopesCollection)
	router.HandleFunc("/broker/envelopes/", s.handleEnvelopesItem)
	router.HandleFunc("/broker/requests", s.handleBrokerRequests)
}

func (s *Server) handleEnvelopesCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleCreateEnvelope(w, r)
	case http.MethodGet:
		s.handleListEnvelopes(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleEnvelopesItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/broker/envelopes/")
	if rest == "" {
		writeError(w, http.StatusNotFound, CodeNotFound, "envelope id required")
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch action {
	case "":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleGetEnvelope(w, r, id)
	case "reply":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleReplyEnvelope(w, r, id)
	default:
		writeError(w, http.StatusNotFound, CodeNotFound, "unknown action "+action)
	}
}

// handleCreateEnvelope services POST /broker/envelopes. ID + CreatedAt
// are server-assigned; every other field round-trips from the request
// body into the Envelope. broker.Service emits the
// broker.envelope_created event on successful persistence.
func (s *Server) handleCreateEnvelope(w http.ResponseWriter, r *http.Request) {
	var req EnvelopeCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	e, err := broker.NewOperations(s.Broker).Create(r.Context(), broker.CreateRequest(req))
	if err != nil {
		writeBrokerOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, envelopeToDTO(*e))
}

// handleListEnvelopes services GET /broker/envelopes. Requires one of
// ?recipient= or ?workflow_id= (optionally scoped further by
// ?correlation_id=). Returning every envelope in the database without
// a filter is deliberately refused — the broker is a mailbox, not a
// browsable feed.
func (s *Server) handleListEnvelopes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	recipient := q.Get("recipient")
	workflow := q.Get("workflow_id")
	correlation := q.Get("correlation_id")

	if recipient == "" && workflow == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"one of recipient or workflow_id is required")
		return
	}
	if recipient != "" && workflow != "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"recipient and workflow_id are mutually exclusive")
		return
	}
	// T05 (messaging vNext): a recipient-scoped mailbox read requires the
	// caller to explicitly claim that identity via ?as= (same-host-trust
	// convention already used throughout /messages/* and /groups/* -- not
	// cryptographic verification, but "you must say who you are" is
	// strictly stronger than the prior zero-claim state, and closes the
	// concrete gap where this endpoint required no identity assertion at
	// all, unlike every other mailbox read in the codebase). The
	// workflow_id-scoped branch remains an operator/debugging query, not a
	// per-recipient mailbox read, and is intentionally left unchanged.
	if recipient != "" {
		as := q.Get("as")
		if as == "" {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "as is required when listing by recipient")
			return
		}
		if as != recipient {
			writeError(w, http.StatusForbidden, CodeForbidden, "as must match recipient")
			return
		}
	}

	var (
		rows []broker.Envelope
		err  error
	)
	if recipient != "" {
		rows, err = s.Broker.ListEnvelopesByRecipient(recipient)
	} else {
		rows, err = s.Broker.ListEnvelopesByWorkflow(workflow, correlation)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	out := make([]EnvelopeDTO, 0, len(rows))
	for _, e := range rows {
		out = append(out, envelopeToDTO(e))
	}
	writeJSON(w, http.StatusOK, EnvelopeListResponse{Envelopes: out})
}

// handleGetEnvelope services GET /broker/envelopes/{id}.
func (s *Server) handleGetEnvelope(w http.ResponseWriter, r *http.Request, id string) {
	// T05 (messaging vNext): require the caller to claim a party to this
	// envelope via ?as= before returning it -- previously this endpoint
	// required no identity assertion at all (not even self-asserted),
	// unlike every other envelope/message read in the codebase. Same-host
	// trust convention, not cryptographic verification; see broker.go's
	// handleListEnvelopes for the identical rationale.
	as := r.URL.Query().Get("as")
	if as == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "as is required")
		return
	}
	e, err := s.Broker.GetEnvelope(id)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			writeError(w, http.StatusNotFound, CodeNotFound, "envelope not found")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	if as != e.Sender && as != e.Recipient {
		writeError(w, http.StatusForbidden, CodeForbidden, "as must be the envelope's sender or recipient")
		return
	}
	writeJSON(w, http.StatusOK, envelopeToDTO(*e))
}

// handleReplyEnvelope services POST /broker/envelopes/{id}/reply. The
// existing envelope is fetched to copy the correlation ID and swap
// sender/recipient on the reply. Body is a regular EnvelopeCreateRequest
// — its sender/recipient fields are ignored (server derives them from
// the original) but the rest carry through.
func (s *Server) handleReplyEnvelope(w http.ResponseWriter, r *http.Request, id string) {
	operation := broker.NewOperations(s.Broker)
	original, err := operation.Original(id)
	if err != nil {
		writeBrokerOperationError(w, err)
		return
	}

	var req EnvelopeCreateRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
			return
		}
	}

	reply, err := operation.Reply(r.Context(), *original, broker.CreateRequest(req))
	if err != nil {
		writeBrokerOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, envelopeToDTO(*reply))
}

// handleBrokerRequests services POST /broker/requests.
//
// Behavior:
//   - ?wait=false (or omitted with no wait): creates the request envelope and
//     returns 202 with the envelope DTO (async mode, caller polls/subscribes).
//   - ?wait=true (default when ?wait is absent but ?timeout is present, OR when
//     wait is explicitly true): creates the request, then blocks until a
//     matching response arrives or the timeout elapses (default 30s). On
//     success returns 200 with the *response* envelope. On timeout returns 504.
//
// The server assigns the correlation_id for all request envelopes regardless
// of mode (per ADR 0018).
func (s *Server) handleBrokerRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}

	q := r.URL.Query()
	opts, err := broker.ParseRequestOptions(q.Get("wait"), q.Get("timeout"))
	if err != nil {
		writeBrokerOperationError(w, err)
		return
	}

	var req EnvelopeCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
		return
	}

	operation := broker.NewOperations(s.Broker)
	e, err := operation.Request(r.Context(), broker.CreateRequest(req))
	if err != nil {
		writeBrokerOperationError(w, err)
		return
	}
	if !opts.Blocking {
		w.Header().Set("X-Correlation-Id", e.CorrelationID)
		writeJSON(w, http.StatusAccepted, envelopeToDTO(*e))
		return
	}
	response, err := operation.Wait(r.Context(), e.CorrelationID, opts.Timeout)
	if err != nil {
		writeBrokerOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, envelopeToDTO(*response))
}

func writeBrokerOperationError(w http.ResponseWriter, err error) {
	status, code, message := http.StatusInternalServerError, CodeInternalError, err.Error()
	var failure *broker.OperationError
	if errors.As(err, &failure) {
		code, message = failure.Code, failure.Message
		switch code {
		case CodeInvalidRequest:
			status = http.StatusBadRequest
		case CodeNotFound:
			status = http.StatusNotFound
		case "timeout":
			status = http.StatusGatewayTimeout
		}
	}
	writeError(w, status, code, message)
}
