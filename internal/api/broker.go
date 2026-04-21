package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/chrispian/agent-mux/internal/broker"
)

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
}

// EnvelopeCreateRequest is the JSON body for POST /broker/envelopes.
// Server-set fields (ID, CreatedAt) are omitted; every other field is
// optional and round-trips verbatim into broker.Envelope.
type EnvelopeCreateRequest struct {
	Sender        string `json:"sender,omitempty"`
	Recipient     string `json:"recipient,omitempty"`
	WorkflowID    string `json:"workflow_id,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
	MessageType   string `json:"message_type,omitempty"`
	Priority      int    `json:"priority,omitempty"`
	Payload       string `json:"payload,omitempty"`
	AuditJSON     string `json:"audit_json,omitempty"`
}

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

func (s *Server) registerBrokerRoutes(mux *http.ServeMux) {
	if s.Broker == nil {
		return
	}
	mux.HandleFunc("/broker/envelopes", s.handleEnvelopesCollection)
	mux.HandleFunc("/broker/envelopes/", s.handleEnvelopesItem)
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
	id, err := uuid.NewV7()
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "generate id: "+err.Error())
		return
	}
	// Validate message_type at the API boundary before passing to service.
	if req.MessageType != "" && !broker.IsValidMessageType(req.MessageType) {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"unknown message_type "+strconv.Quote(req.MessageType)+"; valid: "+strings.Join(broker.ValidMessageTypes(), ", "))
		return
	}

	// For request envelopes the server assigns the correlation_id (UUIDv7)
	// so responses can be matched deterministically. Callers MUST NOT set it.
	correlationID := req.CorrelationID
	if req.MessageType == broker.TypeRequest {
		corrID, err := uuid.NewV7()
		if err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternalError, "generate correlation_id: "+err.Error())
			return
		}
		correlationID = corrID.String()
	}

	e := broker.Envelope{
		ID:            id.String(),
		Sender:        req.Sender,
		Recipient:     req.Recipient,
		WorkflowID:    req.WorkflowID,
		CorrelationID: correlationID,
		MessageType:   req.MessageType,
		Priority:      req.Priority,
		Payload:       req.Payload,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		AuditJSON:     req.AuditJSON,
	}
	if err := s.Broker.CreateEnvelope(r.Context(), e); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "correlation_id") {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, msg)
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, msg)
		return
	}
	writeJSON(w, http.StatusCreated, envelopeToDTO(e))
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
func (s *Server) handleGetEnvelope(w http.ResponseWriter, _ *http.Request, id string) {
	e, err := s.Broker.GetEnvelope(id)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			writeError(w, http.StatusNotFound, CodeNotFound, "envelope not found")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
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
	original, err := s.Broker.GetEnvelope(id)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			writeError(w, http.StatusNotFound, CodeNotFound, "envelope not found")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}

	var req EnvelopeCreateRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
			return
		}
	}

	replyID, err := uuid.NewV7()
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "generate id: "+err.Error())
		return
	}

	// Correlation ID propagates from the original — if the original
	// already carries one, preserve that thread; otherwise the original
	// itself seeds the conversation.
	corrID := original.CorrelationID
	if corrID == "" {
		corrID = original.ID
	}

	reply := broker.Envelope{
		ID:            replyID.String(),
		Sender:        original.Recipient,
		Recipient:     original.Sender,
		WorkflowID:    original.WorkflowID,
		CorrelationID: corrID,
		MessageType:   req.MessageType,
		Priority:      req.Priority,
		Payload:       req.Payload,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		AuditJSON:     req.AuditJSON,
	}
	if err := s.Broker.ReplyEnvelope(r.Context(), reply); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, envelopeToDTO(reply))
}
