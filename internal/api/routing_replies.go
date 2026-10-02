package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/channels"
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
	// Interrupt says what interrupt:true did: "cancelled", or
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
	case errors.Is(err, ErrReplyIdempotencyConflict):
		writeError(w, http.StatusConflict, CodeIdempotencyConflict, err.Error())
	case errors.Is(err, ErrRoutingRepliesNotWired):
		writeError(w, http.StatusNotImplemented, CodeNotImplemented, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
	}
}

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
	var in replyBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid body: "+err.Error())
		return
	}
	s.submitReply(w, r, id, in.Body, in.Interrupt, "")
}

func (s *Server) submitReply(w http.ResponseWriter, r *http.Request, parentID, body string, interrupt bool, fallbackCaller string) {
	caller, verified, err := replyCaller(r, fallbackCaller)
	if err != nil {
		writeReplyError(w, err)
		return
	}
	receipt, err := s.RoutingReplies.SubmitRoutingReply(r.Context(), RoutingReplyRequest{
		ParentID: parentID, Body: body, Interrupt: interrupt, Caller: caller, Verified: verified,
		IdempotencyKey: strings.TrimSpace(r.Header.Get("Idempotency-Key")),
	})
	if err != nil {
		writeReplyError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, receipt)
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

// sendRoutedReply serves POST /messages with in_reply_to naming a routed
// message: the envelope is not delivered to an inbox; its text is queued for
// the sender session exactly as POST /messages/{id}/reply would. The response
// is the reply receipt (202), not the stored envelope.
func (s *Server) sendRoutedReply(w http.ResponseWriter, r *http.Request, env messaging.Envelope, parent messaging.Envelope) {
	if env.To != (messaging.Address{}) && env.To != parent.From {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"a reply to a routed message is delivered to its sender session "+parent.From.URN()+"; omit to or set it to that address")
		return
	}
	s.submitReply(w, r, parent.ID, replyText(env.Payload), false, env.From.URN())
}
