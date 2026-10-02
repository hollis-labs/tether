package broker

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CreateRequest carries caller-authored envelope fields. Identity, timestamps
// and request correlation IDs are assigned by Operations.
type CreateRequest struct {
	Sender        string `json:"sender,omitempty"`
	Recipient     string `json:"recipient,omitempty"`
	WorkflowID    string `json:"workflow_id,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
	MessageType   string `json:"message_type,omitempty"`
	Priority      int    `json:"priority,omitempty"`
	Payload       string `json:"payload,omitempty"`
	AuditJSON     string `json:"audit_json,omitempty"`
}

// OperationBackend combines the existing broker write and correlation seams.
// Reads remain a narrow domain-envelope interface, without a raw store handle.
type OperationBackend interface {
	CreateEnvelope(context.Context, Envelope) error
	ReplyEnvelope(context.Context, Envelope) error
	GetEnvelope(string) (*Envelope, error)
	WaitForResponse(context.Context, string) (*Envelope, error)
}

// OperationError preserves the legacy public failure code and message.
type OperationError struct{ Code, Message string }

func (e *OperationError) Error() string { return e.Message }

// Operations owns server-assigned identity and request/reply correlation policy.
// Service continues to own persistence-before-publish and dispatcher delivery.
type Operations struct {
	backend OperationBackend
	newID   func() (uuid.UUID, error)
	now     func() time.Time
}

func NewOperations(backend OperationBackend) *Operations {
	return &Operations{backend: backend, newID: uuid.NewV7, now: time.Now}
}
func failure(code, message string) error { return &OperationError{Code: code, Message: message} }
func (o *Operations) id(label string) (string, error) {
	id, err := o.newID()
	if err != nil {
		return "", failure("internal_error", "generate "+label+": "+err.Error())
	}
	return id.String(), nil
}
func (o *Operations) envelope(req CreateRequest, id, correlation string) Envelope {
	return Envelope{ID: id, Sender: req.Sender, Recipient: req.Recipient, WorkflowID: req.WorkflowID, CorrelationID: correlation, MessageType: req.MessageType, Priority: req.Priority, Payload: req.Payload, CreatedAt: o.now().UTC().Format(time.RFC3339), AuditJSON: req.AuditJSON}
}

// Create assigns a fresh correlation ID only for request envelopes. The ID
// generation and validation order deliberately retain the legacy API behavior.
func (o *Operations) Create(ctx context.Context, req CreateRequest) (*Envelope, error) {
	id, err := o.id("id")
	if err != nil {
		return nil, err
	}
	if req.MessageType != "" && !IsValidMessageType(req.MessageType) {
		return nil, failure("invalid_request", "unknown message_type "+strconv.Quote(req.MessageType)+"; valid: "+strings.Join(ValidMessageTypes(), ", "))
	}
	correlation := req.CorrelationID
	if req.MessageType == TypeRequest {
		correlation, err = o.id("correlation_id")
		if err != nil {
			return nil, err
		}
	}
	e := o.envelope(req, id, correlation)
	if err := o.backend.CreateEnvelope(ctx, e); err != nil {
		code := "internal_error"
		if strings.Contains(err.Error(), "correlation_id") {
			code = "invalid_request"
		}
		return nil, failure(code, err.Error())
	}
	return &e, nil
}

// Original loads a reply's original before the transport decodes the reply
// body. Missing-original failures therefore still win over malformed JSON.
func (o *Operations) Original(id string) (*Envelope, error) {
	original, err := o.backend.GetEnvelope(id)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return nil, failure("not_found", "envelope not found")
		}
		return nil, failure("internal_error", err.Error())
	}
	return original, nil
}

// Reply preserves an existing thread or seeds one with the original ID, swaps
// the parties and ignores caller-authored sender, recipient and correlation.
func (o *Operations) Reply(ctx context.Context, original Envelope, req CreateRequest) (*Envelope, error) {
	id, err := o.id("id")
	if err != nil {
		return nil, err
	}
	correlation := original.CorrelationID
	if correlation == "" {
		correlation = original.ID
	}
	req.Sender, req.Recipient, req.WorkflowID = original.Recipient, original.Sender, original.WorkflowID
	e := o.envelope(req, id, correlation)
	if err := o.backend.ReplyEnvelope(ctx, e); err != nil {
		return nil, failure("internal_error", err.Error())
	}
	return &e, nil
}

// Request always forces the request type and replaces caller correlation,
// independent of whether the transport will wait or return asynchronously.
func (o *Operations) Request(ctx context.Context, req CreateRequest) (*Envelope, error) {
	req.MessageType = TypeRequest
	id, err := o.id("id")
	if err != nil {
		return nil, err
	}
	correlation, err := o.id("correlation_id")
	if err != nil {
		return nil, err
	}
	e := o.envelope(req, id, correlation)
	if err := o.backend.CreateEnvelope(ctx, e); err != nil {
		return nil, failure("internal_error", err.Error())
	}
	return &e, nil
}

// DefaultRequestTimeout is the legacy request/reply wait budget.
const DefaultRequestTimeout = 30 * time.Second

// RequestOptions preserves the legacy wait/timeout selection, including an
// explicit non-true wait suppressing the timeout-implied blocking default.
type RequestOptions struct {
	Blocking bool
	Timeout  time.Duration
}

func ParseRequestOptions(wait, timeout string) (RequestOptions, error) {
	opts := RequestOptions{Blocking: wait == "true" || wait == "1" || (timeout != "" && wait == ""), Timeout: DefaultRequestTimeout}
	if timeout != "" {
		d, err := time.ParseDuration(timeout)
		if err != nil {
			return RequestOptions{}, failure("invalid_request", "invalid timeout: "+err.Error())
		}
		opts.Timeout = d
	}
	return opts, nil
}

// Wait retains the legacy public timeout failure for every dispatcher error.
func (o *Operations) Wait(ctx context.Context, correlation string, timeout time.Duration) (*Envelope, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := o.backend.WaitForResponse(waitCtx, correlation)
	if err != nil {
		return nil, failure("timeout", "no response received within "+timeout.String())
	}
	return response, nil
}
