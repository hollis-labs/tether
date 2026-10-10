package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/store"
)

// Codes added for reply-to-sender (CW-20261002-0065).
const (
	// CodeReplyTargetNotSession (400): the message being replied to was not
	// sent by a Tether session, so there is no session to deliver to.
	CodeReplyTargetNotSession = "reply_target_not_a_session"
	// CodeInterruptUnsupported (409): interrupt:true was asked of a runtime
	// that cannot cancel a turn. The reply was not accepted.
	CodeInterruptUnsupported = "interrupt_unsupported"
	// CodeTurnNotYetStarted (409): the session's turn was submitted but the
	// runtime has not started it, so there is nothing safe to cancel yet.
	// Nothing was queued; retry.
	CodeTurnNotYetStarted = "turn_not_yet_started"
	// CodeTurnFeedUnavailable (409): the target session's runtime has no turn
	// lifecycle (a PTY), so Tether cannot tell when it is idle; nothing was queued.
	CodeTurnFeedUnavailable = "turn_feed_unavailable"
	// CodeReplyNotMailbox (400): a mailbox verb (cancel, consume, read, archive,
	// claim, ack, nack, redrive) was aimed at a reply. A reply's delivery state is
	// read from GET /messages/{id}/delivery and nowhere else.
	CodeReplyNotMailbox = "reply_not_mailbox"
)

// Errors the reply service returns; handlers map them with writeReplyError.
var (
	ErrReplyInvalid              = errors.New("invalid reply")
	ErrReplyTooLarge             = errors.New("reply body too large")
	ErrReplyParentNotFound       = errors.New("message to reply to not found")
	ErrReplyNotFound             = errors.New("reply not found")
	ErrReplyTargetNotSession     = errors.New("the message was not sent by a session")
	ErrReplyForbidden            = errors.New("reply not permitted")
	ErrReplyInterruptUnsupported = errors.New("this session's runtime cannot interrupt a turn")
	ErrReplyTurnNotStarted       = errors.New("the session's turn has not started yet")
	ErrReplyNoTurnFeed           = errors.New("the session's runtime reports no turn lifecycle, so Tether cannot tell when it is idle")
	ErrReplyIdempotencyConflict  = errors.New("idempotency key reused with a different reply")
	ErrRoutingRepliesNotWired    = errors.New("reply routing is not running in this daemon")
)

// RoutingReplyService is the reply-to-sender surface. *app.Service implements it.
type RoutingReplyService interface {
	SubmitRoutingReply(ctx context.Context, req RoutingReplyRequest) (RoutingReplyReceipt, error)
	RoutingReplyDelivery(ctx context.Context, replyID string) (RoutingReplyDelivery, error)
}

// RoutingReplyRequest is one reply to a routed message.
type RoutingReplyRequest struct {
	ParentID       string
	Body           string
	Interrupt      bool
	IdempotencyKey string
	// Caller is the verified principal when the identity middleware
	// authenticated one, else the self-asserted ?as= identity (Verified
	// false), exactly as channels treat it (observe mode, ADR 0045).
	Caller   identity.Principal
	Verified bool
}

// RoutingReplyReceipt is the 202 body: the reply was accepted and queued.
type RoutingReplyReceipt struct {
	ReplyID         string `json:"reply_id"`
	ParentID        string `json:"parent_id"`
	State           string `json:"state"`
	TargetSessionID string `json:"target_session_id"`
	// Interrupt says what interrupt:true did: the turn_output stop_reason value
	// for an interrupted turn (llmtypes.StopReasonCancelled), or
	// "no_turn_in_progress" / "turn_superseded" / "session_not_running" when
	// there was nothing to cancel and the reply is delivered as a plain next
	// turn. Empty when interrupt was not requested.
	Interrupt string `json:"interrupt,omitempty"`
	// Duplicate is true when the Idempotency-Key matched an earlier reply.
	Duplicate bool `json:"duplicate,omitempty"`
}

// RoutingReplyDelivery is what consumers read to learn where a reply ended up.
type RoutingReplyDelivery struct {
	ReplyID              string     `json:"reply_id"`
	ParentID             string     `json:"parent_id"`
	State                string     `json:"state"`
	Reason               string     `json:"reason,omitempty"`
	Detail               string     `json:"detail,omitempty"`
	OriginalSessionID    string     `json:"original_session_id"`
	TargetSessionID      string     `json:"target_session_id"`
	DeliveredToSessionID string     `json:"delivered_to_session_id,omitempty"`
	InterruptRequested   bool       `json:"interrupt_requested"`
	Attempts             int        `json:"attempts"`
	NextAttemptAt        *time.Time `json:"next_attempt_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
	SettledAt            *time.Time `json:"settled_at,omitempty"`
}

func writeReplyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrReplyInvalid):
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
	case errors.Is(err, ErrReplyTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, err.Error())
	case errors.Is(err, ErrReplyParentNotFound), errors.Is(err, ErrReplyNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
	case errors.Is(err, ErrReplyTargetNotSession):
		writeError(w, http.StatusBadRequest, CodeReplyTargetNotSession, err.Error())
	case errors.Is(err, ErrReplyForbidden):
		writeError(w, http.StatusForbidden, CodeForbidden, err.Error())
	case errors.Is(err, ErrReplyInterruptUnsupported):
		writeError(w, http.StatusConflict, CodeInterruptUnsupported, err.Error())
	case errors.Is(err, ErrReplyTurnNotStarted):
		writeError(w, http.StatusConflict, CodeTurnNotYetStarted, err.Error())
	case errors.Is(err, ErrReplyNoTurnFeed):
		writeError(w, http.StatusConflict, CodeTurnFeedUnavailable, err.Error())
	case errors.Is(err, store.ErrRoutingReplyNotMailbox):
		writeError(w, http.StatusBadRequest, CodeReplyNotMailbox, err.Error())
	case errors.Is(err, ErrReplyIdempotencyConflict):
		writeError(w, http.StatusConflict, CodeIdempotencyConflict, err.Error())
	case errors.Is(err, ErrRoutingRepliesNotWired):
		writeError(w, http.StatusNotImplemented, CodeNotImplemented, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
	}
}

// maxReplyRequestBytes caps the JSON request. The reply text is limited to 128 KiB
// by the service; this leaves room for escaping and rejects anything absurd.
const maxReplyRequestBytes = 1 << 20

type replyBody struct {
	Body      string `json:"body"`
	Interrupt bool   `json:"interrupt,omitempty"`
}

// POST /messages/{id}/reply?as=<caller urn>
// Body: {"body": "...", "interrupt": false}
//
// Queues body for delivery to the session that sent message {id}, as that
// session's next turn at its next idle boundary. 202 with the receipt.
func (s *Server) handleMessageReply(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	if s.RoutingReplies == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "reply routing is not enabled")
		return
	}
	// Read at most the cap plus one byte so an oversized request is a 413, not a
	// decode error: the text itself is limited more tightly by the service.
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxReplyRequestBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid body: "+err.Error())
		return
	}
	if len(raw) > maxReplyRequestBytes {
		writeError(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, fmt.Sprintf("request body over %d bytes", maxReplyRequestBytes))
		return
	}
	var in replyBody
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields() // a misspelled field name must not silently queue a plain reply
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid body: "+err.Error())
		return
	}
	if receipt, ok := s.submitReply(w, r, id, in.Body, in.Interrupt, ""); ok {
		writeJSON(w, http.StatusAccepted, receipt)
	}
}

// submitReply submits the reply and reports the receipt, or writes the error
// response itself and reports false.
func (s *Server) submitReply(w http.ResponseWriter, r *http.Request, parentID, body string, interrupt bool, fallbackCaller string) (RoutingReplyReceipt, bool) {
	caller, verified, err := replyCaller(r, fallbackCaller)
	if err != nil {
		writeReplyError(w, err)
		return RoutingReplyReceipt{}, false
	}
	key, err := principalIdempotencyKey(r.Context(), strings.TrimSpace(r.Header.Get("Idempotency-Key")))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "verified device required")
		return RoutingReplyReceipt{}, false
	}
	receipt, err := s.RoutingReplies.SubmitRoutingReply(r.Context(), RoutingReplyRequest{
		ParentID: parentID, Body: body, Interrupt: interrupt, Caller: caller, Verified: verified,
		IdempotencyKey: key,
	})
	if err != nil {
		writeReplyError(w, err)
		return RoutingReplyReceipt{}, false
	}
	return receipt, true
}

// replyCaller is the verified principal, else the self-asserted ?as= identity
// (or fallback, the envelope's own sender, when ?as= is absent).
func replyCaller(r *http.Request, fallback string) (identity.Principal, bool, error) {
	if p, ok := identity.FromContext(r.Context()); ok {
		return p, true, nil
	}
	as := r.URL.Query().Get("as")
	if as == "" {
		as = fallback
	}
	if _, err := messaging.ParseURN(as); err != nil {
		return identity.Principal{}, false, errors.Join(ErrReplyInvalid, errors.New("a valid caller identity (as) is required"))
	}
	return identity.Principal{ID: as}, false, nil
}

// GET /messages/{reply_id}/delivery
func (s *Server) handleMessageReplyDelivery(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	if s.RoutingReplies == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "reply routing is not enabled")
		return
	}
	d, err := s.RoutingReplies.RoutingReplyDelivery(r.Context(), id)
	if err != nil {
		writeReplyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// routedReplyParent reports whether replying to env is a reply-to-sender: the
// message was published to a channel by a local session. A mailbox message
// that happens to carry a session sender keeps its ordinary delivery.
func routedReplyParent(env messaging.Envelope) bool {
	if _, ok := channels.AddressName(env.To); !ok {
		return false
	}
	return env.From.Kind == messaging.KindSession && env.From.Authority == "local"
}

// replyText is the text of a POST /messages payload sent as a reply: a JSON
// string, or an object's body/text/message, else the raw payload.
func replyText(payload json.RawMessage) string {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" {
		return ""
	}
	var s string
	if json.Unmarshal([]byte(trimmed), &s) == nil {
		return s
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(trimmed), &obj) == nil {
		for _, key := range []string{"body", "text", "message"} {
			if raw, ok := obj[key]; ok {
				if json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != "" {
					return s
				}
			}
		}
	}
	return trimmed
}

// routedReplyResponse is the 201 body of POST /messages when the envelope was a
// reply to a routed message: the stored reply envelope, exactly the shape an
// ordinary send returns, plus its routing receipt. Existing clients (the MCP
// tool, the CLI) read it as a sent message and need no change.
type routedReplyResponse struct {
	messaging.Envelope
	RoutingReply RoutingReplyReceipt `json:"routing_reply"`
}

// routeReplyEnvelope queues the text of an envelope that replies to a routed
// message and reads the stored reply back. It writes the error response itself
// and reports false on any failure.
func (s *Server) routeReplyEnvelope(w http.ResponseWriter, r *http.Request, env messaging.Envelope, parent messaging.Envelope) (messaging.Envelope, RoutingReplyReceipt, bool) {
	if env.To != (messaging.Address{}) && env.To != parent.From {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"a reply to a routed message is delivered to its sender session "+parent.From.URN()+"; omit to or set it to that address")
		return messaging.Envelope{}, RoutingReplyReceipt{}, false
	}
	receipt, ok := s.submitReply(w, r, parent.ID, replyText(env.Payload), false, env.From.URN())
	if !ok {
		return messaging.Envelope{}, RoutingReplyReceipt{}, false
	}
	stored, err := s.MessageStore.Get(r.Context(), receipt.ReplyID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "reply accepted as "+receipt.ReplyID+" but could not be read back: "+err.Error())
		return messaging.Envelope{}, RoutingReplyReceipt{}, false
	}
	return stored, receipt, true
}

// sendRoutedReply serves POST /messages with in_reply_to naming a routed
// message: the envelope is not delivered to an inbox; its text is queued for
// the sender session exactly as POST /messages/{id}/reply would.
func (s *Server) sendRoutedReply(w http.ResponseWriter, r *http.Request, env messaging.Envelope, parent messaging.Envelope) {
	if stored, receipt, ok := s.routeReplyEnvelope(w, r, env, parent); ok {
		writeJSON(w, http.StatusCreated, routedReplyResponse{Envelope: stored, RoutingReply: receipt})
	}
}

// notifyRoutedReply serves POST /messages/notify with in_reply_to naming a
// routed message. Notify's mailbox wake is the old path, which injects a generic
// reminder turn rather than the reply text, so it does not apply: the reply is
// queued for the sender session like any other, and wake/wake_text/urgency are
// ignored. The response keeps notify's shape (nothing was left unread, nothing
// was woken) and adds the receipt.
func (s *Server) notifyRoutedReply(w http.ResponseWriter, r *http.Request, env messaging.Envelope, parent messaging.Envelope) {
	if stored, receipt, ok := s.routeReplyEnvelope(w, r, env, parent); ok {
		writeJSON(w, http.StatusCreated, messageNotifyResponse{Message: stored, RoutingReply: &receipt})
	}
}

// routedParent returns the message env replies to when that is a routed
// (channel) message and reply routing is on.
func (s *Server) routedParent(r *http.Request, inReplyTo string) (messaging.Envelope, bool) {
	if inReplyTo == "" || s.RoutingReplies == nil {
		return messaging.Envelope{}, false
	}
	parent, err := s.MessageStore.Get(r.Context(), inReplyTo)
	if err != nil || !routedReplyParent(parent) {
		return messaging.Envelope{}, false
	}
	return parent, true
}
