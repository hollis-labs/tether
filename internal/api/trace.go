package api

// trace.go — T09 (messaging vNext, CW-20260906-0040): the structured
// delivery trace the architecture calls for: "Expose structured trace
// joining actor/SESSION/provider mappings/host generations/message/
// thread/delivery attempts with external task/run/commit refs."
//
// Before this file, T09 design research confirmed nothing in the
// codebase joined delivery-core data (go-messaging's Attempts/Receipts,
// already fully implemented there) with Tether's own session/binding
// tables at all -- this is the first such join. Acceptance #1 requires
// the trace answer five specific questions; each is answered by exactly
// one part of the response, named in the field comments below.
//
// Deliberately excluded: the message's own payload/body content. The
// questions this trace answers are all about DELIVERY MECHANICS (who,
// which binding, which host, why retry) -- GET /messages/{id} already
// exposes the payload verbatim for a caller who wants that; duplicating
// it here would be scope creep unrelated to tracing, and keeps this
// endpoint's privacy surface strictly structural (see the privacy note
// on TraceAttempt.HostID below).
//
// Routing: GET /messages/{id}/trace is dispatched from
// handleMessagesItem's existing action switch (messages.go), not a
// separate mux.HandleFunc("/messages/", ...) registration -- that prefix
// is already owned by registerMessageRoutes, and a second registration
// on the same pattern panics ("multiple registrations").

import (
	"context"
	"net/http"
	"time"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/delivery"

	"github.com/hollis-labs/tether/internal/registry"
)

// DeliveryTraceStore is the narrow seam GET /messages/{id}/trace depends
// on. *store.Store satisfies it directly (both methods already exist).
type DeliveryTraceStore interface {
	DeliveryIDForMessage(ctx context.Context, messageID string) (string, bool, error)
	DeliveryStore() delivery.Store
}

// TraceReceipt is one stage-transition event, in the order it happened.
type TraceReceipt struct {
	AttemptID string `json:"attempt_id,omitempty"`
	Stage     string `json:"stage"`
	At        string `json:"at"`
	Detail    string `json:"detail,omitempty"`
}

// TraceAttempt is one Claim/Ack/Nack cycle. Answers "which host accepted"
// (HostID, cross-referenced from the runtime binding live at
// BindingGeneration -- best-effort: only populated when that generation
// is still resolvable via ListBindingsForTarget, since bindings are
// fenced/superseded, never deleted) and "why retry/expiry occurred"
// (Error/Retryable/NextAttemptAt, taken straight from the attempt's own
// terminal Nack, not reconstructed).
//
// Privacy note: HostID here is the operator/bridge-supplied identifier
// from RuntimeBinding.HostID (e.g. "local" or a bridge's own chosen
// string) -- an infrastructure identifier this trace exists specifically
// to surface (acceptance #1: "which host accepted"), not registry
// Profile data. No Profile field (Callback, HostAddress, KindMeta) is
// ever included in a trace.
type TraceAttempt struct {
	AttemptID         string `json:"attempt_id"`
	Holder            string `json:"holder"` // the session id this attempt was claimed for
	BindingGeneration int64  `json:"binding_generation,omitempty"`
	HostID            string `json:"host_id,omitempty"`
	BindingVisibility string `json:"binding_visibility,omitempty"`
	AcquiredAt        string `json:"acquired_at"`
	Stage             string `json:"stage"`
	HostAcceptedAt    string `json:"host_accepted_at,omitempty"`
	TurnSubmittedAt   string `json:"turn_submitted_at,omitempty"`
	ConsumedAt        string `json:"consumed_at,omitempty"`
	FailedAt          string `json:"failed_at,omitempty"`
	Error             string `json:"error,omitempty"`
	Retryable         bool   `json:"retryable,omitempty"`
	NextAttemptAt     string `json:"next_attempt_at,omitempty"`
}

// TraceResponse is the full delivery trace for one message.
type TraceResponse struct {
	MessageID string `json:"message_id"`
	// From/To answer "who sent to whom."
	From     string `json:"from"`
	To       string `json:"to"`
	Kind     string `json:"kind"`
	ThreadID string `json:"thread_id,omitempty"`

	DeliveryID       string `json:"delivery_id,omitempty"`
	Status           string `json:"status,omitempty"`
	AttemptCount     int    `json:"attempt_count,omitempty"`
	DeadLetterReason string `json:"dead_letter_reason,omitempty"`

	// Attempts answers "which host accepted, which turn was submitted."
	Attempts []TraceAttempt `json:"attempts,omitempty"`
	// Receipts is the raw ordered stage-transition log underlying
	// Attempts -- kept alongside since Attempts summarizes one row per
	// claim while Receipts shows the exact sequence including any
	// implicit persisted/lease_acquired stages between claims.
	Receipts []TraceReceipt `json:"receipts,omitempty"`
}

// handleMessageTrace services GET /messages/{id}/trace. Same-host,
// self-asserted trust model (ADR 0045) -- the trace is operator-facing
// diagnostic data, not a recipient-scoped mailbox read, so it carries no
// ?as= requirement of its own (matching the pre-existing precedent that
// operator/debugging queries -- e.g. the workflow_id branch of legacy
// GET /broker/envelopes -- are not per-recipient reads).
func (s *Server) handleMessageTrace(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	if s.DeliveryTrace == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "delivery trace not configured")
		return
	}
	env, err := s.MessageStore.Get(r.Context(), id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, CodeNotFound, "message not found")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}

	out := TraceResponse{
		MessageID: env.ID,
		From:      env.From.URN(),
		To:        env.To.URN(),
		Kind:      string(env.Kind),
		ThreadID:  env.ThreadID,
	}

	deliveryID, ok, err := s.DeliveryTrace.DeliveryIDForMessage(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	if !ok {
		// Pre-T03 legacy message: no delivery-core tracking exists.
		// Honest partial trace (who sent to whom) rather than a 404 --
		// the message itself is real, tracing detail just isn't
		// available for it.
		writeJSON(w, http.StatusOK, out)
		return
	}
	out.DeliveryID = deliveryID

	ds := s.DeliveryTrace.DeliveryStore()
	rd, err := ds.GetDelivery(r.Context(), delivery.DeliveryID(deliveryID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	out.Status = string(rd.Status)
	out.AttemptCount = rd.AttemptCount
	out.DeadLetterReason = rd.DeadLetterReason

	attempts, err := ds.Attempts(r.Context(), delivery.DeliveryID(deliveryID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	for _, a := range attempts {
		ta := TraceAttempt{
			AttemptID:         string(a.ID),
			Holder:            a.Holder,
			BindingGeneration: a.BindingGeneration,
			AcquiredAt:        formatTraceTime(a.AcquiredAt),
			Stage:             string(a.Stage),
			HostAcceptedAt:    formatTraceTime(a.HostAcceptedAt),
			TurnSubmittedAt:   formatTraceTime(a.TurnSubmittedAt),
			ConsumedAt:        formatTraceTime(a.ConsumedAt),
			FailedAt:          formatTraceTime(a.FailedAt),
			Error:             a.Error,
			Retryable:         a.Retryable,
			NextAttemptAt:     formatTraceTime(a.NextAttemptAt),
		}
		if a.BindingGeneration != 0 && s.Registry != nil && env.To.Kind == messaging.KindAgent {
			if b, found := s.findBindingGeneration(r.Context(), env.To.ID, a.BindingGeneration); found {
				ta.HostID = b.HostID
				ta.BindingVisibility = string(b.Visibility)
			}
		}
		out.Attempts = append(out.Attempts, ta)
	}

	receipts, err := ds.Receipts(r.Context(), delivery.DeliveryID(deliveryID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	for _, rcpt := range receipts {
		out.Receipts = append(out.Receipts, TraceReceipt{
			AttemptID: string(rcpt.AttemptID),
			Stage:     string(rcpt.Stage),
			At:        formatTraceTime(rcpt.At),
			Detail:    rcpt.Detail,
		})
	}

	writeJSON(w, http.StatusOK, out)
}

// findBindingGeneration looks up the specific historical binding
// generation for a logical agent -- bindings are fenced/superseded, not
// deleted, so a prior generation remains resolvable via
// ListBindingsForTarget's full audit history even once superseded.
// Best-effort: returns ok=false (not an error) when the generation can't
// be found, e.g. for a delivery pre-dating T09's generation-capture fix.
func (s *Server) findBindingGeneration(ctx context.Context, logicalAgentID string, generation int64) (registry.RuntimeBinding, bool) {
	target := registry.LogicalAgentBindingTarget(logicalAgentID)
	all, err := s.Registry.ListBindingsForTarget(ctx, target)
	if err != nil {
		return registry.RuntimeBinding{}, false
	}
	for _, b := range all {
		if b.Generation == generation {
			return b, true
		}
	}
	return registry.RuntimeBinding{}, false
}

// formatTraceTime renders t as RFC3339 with millisecond precision, or ""
// for the zero value -- go-messaging leaves not-yet-reached timestamp
// fields (e.g. ConsumedAt on an attempt still in flight) as time.Time{},
// and an empty string is a cleaner wire signal than "0001-01-01...".
func formatTraceTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02T15:04:05.000Z07:00")
}
