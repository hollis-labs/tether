package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/delivery"
	"github.com/hollis-labs/tether/internal/store"
)

// MessageStore is the seam the /messages/* handlers depend on.
// *store.Store.MessagingStore() satisfies it. It is the non-destructive
// InboxStore superset of messaging.Store, so the handlers can serve both
// the atomic-delivery pull (Inbox) and the repeatable List/read/archive
// surface.
type MessageStore interface {
	store.InboxStore
}

// DeliveryClaimer is the seam the /messages/{id}/claim|ack|nack handlers
// depend on (T07, messaging vNext) -- durable, authorized claim/ack/nack
// for a caller pulling its own mailbox (a published-local bridge, or any
// other caller) rather than Tether pushing a wake. *store.Store satisfies
// it directly. Optional: when Server.DeliveryClaims is nil, these three
// actions respond 404, matching every other optional-dependency surface
// in this package (Registry, Groups, Broker, ...).
type DeliveryClaimer interface {
	ClaimMessageDelivery(ctx context.Context, id string, recipient messaging.Address, holder string, leaseDuration time.Duration) (messaging.Envelope, delivery.LeaseRef, error)
	AckMessageDelivery(ctx context.Context, lease delivery.LeaseRef, stage delivery.ReceiptStage) (delivery.RecipientDelivery, delivery.Attempt, error)
	NackMessageDelivery(ctx context.Context, lease delivery.LeaseRef, retryable bool, errMsg string, nextAttemptIn time.Duration) (delivery.RecipientDelivery, delivery.Attempt, error)
	DeliveryIDForMessage(ctx context.Context, messageID string) (string, bool, error)
}

// registerMessageRoutes mounts the go-messaging-native HTTP surface.
// No-op when MessageStore is nil.
func (s *Server) registerMessageRoutes(mux *http.ServeMux) {
	if s.MessageStore == nil {
		return
	}
	mux.HandleFunc("/messages", s.handleMessagesCollection)
	mux.HandleFunc("/messages/notify", s.handleMessageNotify)
	mux.HandleFunc("/messages/request", s.handleMessageRequest)
	mux.HandleFunc("/messages/subscribe", s.handleMessagesSubscribe)
	mux.HandleFunc("/messages/inbox", s.handleMessagesInbox)
	mux.HandleFunc("/messages/list", s.handleMessagesList)
	mux.HandleFunc("/messages/thread/", s.handleMessagesThread)
	mux.HandleFunc("/messages/", s.handleMessagesItem)
}

type messageNotifyRequest struct {
	Kind        messaging.Kind    `json:"kind"`
	Channel     messaging.Channel `json:"channel,omitempty"`
	From        messaging.Address `json:"from"`
	To          messaging.Address `json:"to"`
	ThreadID    string            `json:"thread_id,omitempty"`
	InReplyTo   string            `json:"in_reply_to,omitempty"`
	Payload     json.RawMessage   `json:"payload,omitempty"`
	ContentType string            `json:"content_type,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Urgency     string            `json:"urgency,omitempty"`
	SessionID   string            `json:"session_id,omitempty"`
	Wake        *bool             `json:"wake,omitempty"`
	WakeText    string            `json:"wake_text,omitempty"`
}

type messageNotifyResponse struct {
	Message       messaging.Envelope `json:"message"`
	UnreadCount   int                `json:"unread_count"`
	WakeAttempted bool               `json:"wake_attempted"`
	WakeDelivered bool               `json:"wake_delivered"`
	SessionID     string             `json:"session_id,omitempty"`
	WakeError     string             `json:"wake_error,omitempty"`
	// WakeReason carries an observational, non-error wake disposition
	// (T06, messaging vNext): "busy", "offline", "offline-race",
	// "stale-generation", or "claim-unavailable". The delivery was
	// released for retry via the shared pump (internal/app/wake.go's
	// RunWakeSweep) in every one of these cases, not lost. Distinct from
	// WakeError, which is reserved for an actual failure (SendTurn itself
	// erroring).
	WakeReason string `json:"wake_reason,omitempty"`
}

var validUrgencies = map[string]struct{}{
	"very-low": {},
	"low":      {},
	"normal":   {},
	"high":     {},
}

// POST /messages/notify
//
// Notify is the convenience bridge for "mailbox wake" delivery. It persists a
// normal message envelope, then injects a standardized wake turn into a live
// session when the recipient resolves to one. Delivery is best-effort: storing
// the message is authoritative; wake errors are returned in-band.
func (s *Server) handleMessageNotify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	if s.Service == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "session service not configured")
		return
	}
	var req messageNotifyRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxInputBytes+1)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid body: "+err.Error())
		return
	}
	if req.Kind == "" {
		req.Kind = messaging.MsgKindNotice
	}
	if _, ok := validMessageKinds[req.Kind]; !ok {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			fmt.Sprintf("invalid kind %q; valid: request, response, notice, status_update, handoff, escalation", req.Kind))
		return
	}
	if req.From.IsZero() || req.To.IsZero() {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "from and to are required")
		return
	}
	if req.Urgency == "" {
		req.Urgency = "normal"
	}
	if _, ok := validUrgencies[req.Urgency]; !ok {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid urgency; valid: very-low, low, normal, high")
		return
	}
	if req.Metadata == nil {
		req.Metadata = map[string]string{}
	}
	req.Metadata["urgency"] = req.Urgency
	env := messaging.Envelope{
		Kind:        req.Kind,
		Channel:     req.Channel,
		From:        req.From,
		To:          req.To,
		ThreadID:    req.ThreadID,
		InReplyTo:   req.InReplyTo,
		Payload:     req.Payload,
		ContentType: req.ContentType,
		Metadata:    req.Metadata,
	}
	sent, err := s.MessageStore.Send(r.Context(), env)
	if err != nil {
		if errors.Is(err, messaging.ErrPresetLifecycle) {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}

	unread, countErr := s.unreadCount(r.Context(), sent.To)
	if countErr != nil {
		unread = 1
	}
	res := messageNotifyResponse{Message: sent, UnreadCount: unread}
	wake := true
	if req.Wake != nil {
		wake = *req.Wake
	}
	if wake {
		sessionID, resolveErr := s.resolveNotifySession(r.Context(), req.SessionID, sent.To)
		if resolveErr != nil {
			res.WakeError = resolveErr.Error()
		} else if sessionID != "" {
			res.WakeAttempted = true
			res.SessionID = sessionID
			text := req.WakeText
			if text == "" {
				text = mailboxWakeText(sent, unread, req.Urgency)
			}
			// T06 (messaging vNext): AttemptWake drives a real Claim/Ack/
			// Nack sequence around this wake attempt (host_accepted/
			// turn_submitted receipts, busy/offline/stale-generation
			// handling) instead of calling SendTurn directly -- see
			// internal/app/wake.go.
			outcome := s.Service.AttemptWake(r.Context(), sent.ID, sent.To, sessionID, text)
			res.WakeDelivered = outcome.Delivered
			switch outcome.Reason {
			case "":
				// delivered; nothing more to report.
			case "turn-submit-failed":
				if outcome.Detail != "" {
					res.WakeError = outcome.Detail
				} else {
					res.WakeError = outcome.Reason
				}
			default:
				// Observational, non-error dispositions (busy/offline/
				// stale-generation/claim-unavailable): the delivery was
				// released for retry via the shared pump, not lost or
				// silently redirected to a different session.
				res.WakeReason = outcome.Reason
			}
		}
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) unreadCount(ctx context.Context, to messaging.Address) (int, error) {
	page, err := s.MessageStore.List(ctx, to, store.ListFilter{UnreadOnly: true, IncludeArchived: false, Limit: 1})
	if err != nil {
		return 0, err
	}
	return page.Total, nil
}

func (s *Server) resolveNotifySession(ctx context.Context, explicit string, to messaging.Address) (string, error) {
	if explicit != "" {
		row, err := s.Service.GetSession(explicit)
		if err != nil {
			return "", err
		}
		// T05 (messaging vNext): the explicit session_id override must
		// still correspond to the message's actual recipient -- without
		// this check, a caller could address a message to one URN while
		// waking a completely unrelated session by ID, since sending a
		// message and waking an arbitrary session are different
		// capabilities (architecture: "Publication/wake/admin capabilities
		// differ from permission to send"). msg://session/<..>/<id> must
		// name exactly this session; msg://agent/<..>/<logical_agent_id>
		// must be the logical agent this session belongs to.
		switch to.Kind {
		case messaging.KindSession:
			if to.ID != explicit {
				return "", fmt.Errorf("session_id %s does not match the message recipient session %s", explicit, to.ID)
			}
		case messaging.KindAgent:
			if row.LogicalAgentID != to.ID {
				return "", fmt.Errorf("session_id %s does not belong to recipient logical agent %s", explicit, to.ID)
			}
		default:
			return "", fmt.Errorf("session_id override is not valid for recipient kind %q", to.Kind)
		}
		if _, ok := s.Service.RuntimeHealth(explicit); !ok {
			return "", fmt.Errorf("session %s is not running", explicit)
		}
		return explicit, nil
	}
	if to.Kind == messaging.KindSession {
		if _, ok := s.Service.RuntimeHealth(to.ID); ok {
			return to.ID, nil
		}
		return "", fmt.Errorf("session %s is not running", to.ID)
	}
	if to.Kind != messaging.KindAgent {
		return "", nil
	}
	// T06 (messaging vNext): authoritative actor/session binding
	// resolution replaces the raw newest-running-session scan that used to
	// live here directly -- see internal/app/wake.go's ResolveActorSession
	// (binding-first, with the same newest-running-session heuristic
	// preserved as its documented legacy fallback for actors nobody has
	// explicitly bound yet).
	return s.Service.ResolveActorSession(ctx, to.ID)
}

func mailboxWakeText(env messaging.Envelope, unread int, urgency string) string {
	if unread < 1 {
		unread = 1
	}
	return fmt.Sprintf("**Mailbox wake (daemon-injected)** — you have %d unread message(s) waiting. Latest message: `%s` from `%s` to `%s`, kind `%s`, urgency `%s`.\n\nPlease run your inbox-check procedure, process messages normally, and mark handled messages read or consumed according to your checklist. This is a delivery notification, not an instruction to abandon the current task unless your own procedure says the message is urgent.",
		unread, env.ID, env.From.URN(), env.To.URN(), env.Kind, urgency)
}

func (s *Server) handleMessagesCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleMessageSend(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleMessagesItem(w http.ResponseWriter, r *http.Request) {
	// /messages/inbox → handleMessagesInbox (already registered as exact path)
	// /messages/thread/{id} → handleMessagesThread (registered with prefix)
	rest := strings.TrimPrefix(r.URL.Path, "/messages/")
	if rest == "" {
		writeError(w, http.StatusNotFound, CodeNotFound, "message id required")
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
		switch r.Method {
		case http.MethodGet:
			s.handleMessageGet(w, r, id)
		case http.MethodDelete:
			// DELETE /messages/{id} is a soft-delete: it archives the
			// message for the recipient rather than removing the row.
			s.handleMessageArchive(w, r, id)
		default:
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		}
	case "consume":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleMessageConsume(w, r, id)
	case "cancel":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleMessageCancel(w, r, id)
	case "read":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleMessageMarkRead(w, r, id)
	case "archive":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleMessageArchive(w, r, id)
	case "unarchive":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleMessageUnarchive(w, r, id)
	case "claim":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleMessageClaim(w, r, id)
	case "ack":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleMessageAck(w, r, id)
	case "nack":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleMessageNack(w, r, id)
	case "trace":
		s.handleMessageTrace(w, r, id)
	case "redrive":
		s.handleMessageRedrive(w, r, id)
	case "purge":
		s.handleMessagePurge(w, r, id)
	default:
		writeError(w, http.StatusNotFound, CodeNotFound, "unknown action "+action)
	}
}

// validMessageKinds is the closed set of allowed envelope kinds on the
// /messages surface. MsgKindResponse ("response") is the go-messaging
// wire name; the MCP tools and human docs call it "reply".
var validMessageKinds = map[messaging.Kind]struct{}{
	messaging.MsgKindRequest:      {},
	messaging.MsgKindResponse:     {},
	messaging.MsgKindNotice:       {},
	messaging.MsgKindStatusUpdate: {},
	messaging.MsgKindHandoff:      {},
	messaging.MsgKindEscalation:   {},
}

// POST /messages
// Body: messaging.Envelope (id, created_at, delivered_at, consumed_at are server-assigned/ignored).
func (s *Server) handleMessageSend(w http.ResponseWriter, r *http.Request) {
	var env messaging.Envelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid body: "+err.Error())
		return
	}
	// Validate kind against closed enum. Empty kind is rejected — all new
	// messages must have an explicit kind (backwards-compat empty-kind is
	// only preserved in the legacy /broker surface).
	if _, ok := validMessageKinds[env.Kind]; !ok {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			fmt.Sprintf("invalid kind %q; valid: request, response, notice, status_update, handoff, escalation", env.Kind))
		return
	}
	// Guard: reject caller-set lifecycle fields.
	env.ID = ""
	env.CreatedAt = time.Time{}
	env.DeliveredAt = nil
	env.ConsumedAt = nil

	sent, err := s.MessageStore.Send(r.Context(), env)
	if err != nil {
		if errors.Is(err, messaging.ErrPresetLifecycle) {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sent)
}

// GET /messages/{id}
// T05 (messaging vNext): require the caller to claim a party to this
// message via ?as= before returning it -- previously this endpoint required
// no identity assertion at all, unlike the recipient-scoped actions
// (read/archive/unarchive/consume) on the same file which already required
// ?as=. Same-host trust convention (ADR 0045), not cryptographic
// verification -- see internal/api/broker.go's handleGetEnvelope for the
// identical rationale applied to the legacy broker surface.
func (s *Server) handleMessageGet(w http.ResponseWriter, r *http.Request, id string) {
	as := r.URL.Query().Get("as")
	if as == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "as is required")
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
	if as != env.From.URN() && as != env.To.URN() {
		writeError(w, http.StatusForbidden, CodeForbidden, "as must be the message's sender or recipient")
		return
	}
	writeJSON(w, http.StatusOK, env)
}

// GET /messages/inbox?to=<urn>&as=<urn>[&kind=request,notice][&thread_id=X][&limit=N]
func (s *Server) handleMessagesInbox(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	toURN := q.Get("to")
	if toURN == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "to query param required")
		return
	}
	// T05 (messaging vNext): a mailbox read requires the caller to
	// explicitly claim the mailbox owner's identity via ?as= (ADR 0045).
	as := q.Get("as")
	if as == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "as is required")
		return
	}
	if as != toURN {
		writeError(w, http.StatusForbidden, CodeForbidden, "as must match to")
		return
	}
	to, err := messaging.ParseURN(toURN)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid to URN: "+err.Error())
		return
	}

	var f messaging.Filter
	if ks := q.Get("kind"); ks != "" {
		for _, k := range strings.Split(ks, ",") {
			f.Kind = append(f.Kind, messaging.Kind(strings.TrimSpace(k)))
		}
	}
	f.ThreadID = q.Get("thread_id")

	envs, err := s.MessageStore.Inbox(r.Context(), to, f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": envs})
}

// GET /messages/list?to=<urn>[&kind=request,notice][&thread_id=X]
//
//	[&limit=N][&offset=N]
//	[&include_archived=true][&unread_only=true]
//
// Non-destructive, repeatable listing of a recipient's messages. Unlike
// /messages/inbox this never stamps delivered_at — a UI can poll it
// without consuming the inbox. Archived messages are excluded unless
// include_archived=true. Each message carries read_at/archived_at/
// canceled_at state and a subject/body payload projection.
//
// Pagination: limit is clamped to [1,100] (default 100), offset floored at
// 0. The response includes limit, offset, and total (the count of matching
// messages before paging).
func (s *Server) handleMessagesList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	toURN := q.Get("to")
	if toURN == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "to query param required")
		return
	}
	// T05 (messaging vNext): a mailbox read requires the caller to
	// explicitly claim the mailbox owner's identity via ?as= (ADR 0045).
	as := q.Get("as")
	if as == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "as is required")
		return
	}
	if as != toURN {
		writeError(w, http.StatusForbidden, CodeForbidden, "as must match to")
		return
	}
	to, err := messaging.ParseURN(toURN)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid to URN: "+err.Error())
		return
	}

	var f store.ListFilter
	if ks := q.Get("kind"); ks != "" {
		for _, k := range strings.Split(ks, ",") {
			f.Kind = append(f.Kind, messaging.Kind(strings.TrimSpace(k)))
		}
	}
	f.ThreadID = q.Get("thread_id")
	f.IncludeArchived = q.Get("include_archived") == "true"
	f.UnreadOnly = q.Get("unread_only") == "true"
	if ls := q.Get("limit"); ls != "" {
		if n, convErr := strconv.Atoi(ls); convErr == nil {
			f.Limit = n
		}
	}
	if os := q.Get("offset"); os != "" {
		if n, convErr := strconv.Atoi(os); convErr == nil {
			f.Offset = n
		}
	}

	page, err := s.MessageStore.List(r.Context(), to, f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"messages": page.Messages,
		"count":    len(page.Messages),
		"total":    page.Total,
		"limit":    page.Limit,
		"offset":   page.Offset,
	})
}

// recipientAction is the shared body for the recipient-scoped, idempotent
// state transitions (read / archive / unarchive). Each requires ?as=<urn>
// identifying the recipient and returns 204 on success, 404 when the id is
// absent, and 409 when the caller is not the intended recipient.
func (s *Server) recipientAction(w http.ResponseWriter, r *http.Request, id string,
	fn func(context.Context, string, messaging.Address) error) {
	asURN := r.URL.Query().Get("as")
	if asURN == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "as query param required")
		return
	}
	recipient, err := messaging.ParseURN(asURN)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid as URN: "+err.Error())
		return
	}
	if err := fn(r.Context(), id, recipient); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, CodeNotFound, "message not found")
			return
		}
		if isWrongRecipient(err) {
			writeError(w, http.StatusConflict, CodeConflict, "caller is not the intended recipient")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /messages/{id}/read?as=<urn> — mark a message read (idempotent).
func (s *Server) handleMessageMarkRead(w http.ResponseWriter, r *http.Request, id string) {
	s.recipientAction(w, r, id, s.MessageStore.MarkRead)
}

// POST /messages/{id}/archive?as=<urn> or DELETE /messages/{id}?as=<urn>
// — soft-delete a message (idempotent). Archived messages drop out of
// default /messages/list results.
func (s *Server) handleMessageArchive(w http.ResponseWriter, r *http.Request, id string) {
	s.recipientAction(w, r, id, s.MessageStore.Archive)
}

// POST /messages/{id}/unarchive?as=<urn> — restore an archived message.
func (s *Server) handleMessageUnarchive(w http.ResponseWriter, r *http.Request, id string) {
	s.recipientAction(w, r, id, s.MessageStore.Unarchive)
}

// GET /messages/thread/{thread_id}[?kind=X][&limit=N]
func (s *Server) handleMessagesThread(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	threadID := strings.TrimPrefix(r.URL.Path, "/messages/thread/")
	if threadID == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "thread_id required")
		return
	}
	// T05 (messaging vNext): a thread read requires the caller to claim a
	// party via ?as= (ADR 0045). A thread is a shared request/reply chain
	// (From/To alternate per turn), not a single mailbox, so rather than
	// rejecting outright the response is scoped to only the turns that
	// actually involve the claimed identity -- a legitimate two-party
	// thread participant still sees every turn, since every turn's From or
	// To equals one of the two parties.
	as := r.URL.Query().Get("as")
	if as == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "as is required")
		return
	}

	var f messaging.Filter
	if ks := r.URL.Query().Get("kind"); ks != "" {
		for _, k := range strings.Split(ks, ",") {
			f.Kind = append(f.Kind, messaging.Kind(strings.TrimSpace(k)))
		}
	}

	envs, err := s.MessageStore.Thread(r.Context(), threadID, f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	scoped := make([]messaging.Envelope, 0, len(envs))
	for _, env := range envs {
		if as == env.From.URN() || as == env.To.URN() {
			scoped = append(scoped, env)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": scoped})
}

// POST /messages/{id}/consume?as=<urn>
func (s *Server) handleMessageConsume(w http.ResponseWriter, r *http.Request, id string) {
	asURN := r.URL.Query().Get("as")
	if asURN == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "as query param required")
		return
	}
	recipient, err := messaging.ParseURN(asURN)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid as URN: "+err.Error())
		return
	}
	if err := s.MessageStore.Consume(r.Context(), id, recipient); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, CodeNotFound, "message not found")
			return
		}
		if isWrongRecipient(err) {
			writeError(w, http.StatusConflict, CodeConflict, "caller is not the intended recipient")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /messages/{id}/cancel
func (s *Server) handleMessageCancel(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.MessageStore.Cancel(r.Context(), id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, CodeNotFound, "message not found")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// claimLeaseSeconds bounds a caller-requested claim lease. 0 (unset)
// falls back to a sane default; a caller cannot hold a lease indefinitely
// by requesting an absurd duration -- it just gets capped.
const (
	defaultClaimLeaseSeconds = 30
	maxClaimLeaseSeconds     = 300
)

type messageClaimRequest struct {
	Holder       string `json:"holder"`
	LeaseSeconds int    `json:"lease_seconds"`
}

type messageClaimResponse struct {
	Message   messaging.Envelope `json:"message"`
	Lease     delivery.LeaseRef  `json:"lease"`
	ExpiresIn int                `json:"lease_seconds"`
}

// handleMessageClaim services POST /messages/{id}/claim?as=<urn> (T07,
// messaging vNext): durable, authorized claim for a caller pulling its
// own mailbox on its own initiative -- the counterpart to AttemptWake's
// internal Claim for a Tether-pushed wake. as must be the message's
// actual recipient (ADR 0045's self-asserted convention, same as
// Consume). Body {holder, lease_seconds} is optional; holder defaults to
// the asserted recipient URN.
func (s *Server) handleMessageClaim(w http.ResponseWriter, r *http.Request, id string) {
	if s.DeliveryClaims == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "delivery claims not configured")
		return
	}
	asURN := r.URL.Query().Get("as")
	if asURN == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "as query param required")
		return
	}
	recipient, err := messaging.ParseURN(asURN)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid as URN: "+err.Error())
		return
	}
	var req messageClaimRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, maxInputBytes+1)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
			return
		}
	}
	if req.Holder == "" {
		req.Holder = asURN
	}
	leaseSeconds := req.LeaseSeconds
	if leaseSeconds <= 0 {
		leaseSeconds = defaultClaimLeaseSeconds
	}
	if leaseSeconds > maxClaimLeaseSeconds {
		leaseSeconds = maxClaimLeaseSeconds
	}

	env, lease, err := s.DeliveryClaims.ClaimMessageDelivery(r.Context(), id, recipient, req.Holder, time.Duration(leaseSeconds)*time.Second)
	if err != nil {
		writeClaimError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, messageClaimResponse{Message: env, Lease: lease, ExpiresIn: leaseSeconds})
}

type messageAckRequest struct {
	Lease delivery.LeaseRef     `json:"lease"`
	Stage delivery.ReceiptStage `json:"stage"`
}

// handleMessageAck services POST /messages/{id}/ack?as=<urn> (T07,
// messaging vNext). Body carries the exact LeaseRef handleMessageClaim
// returned and the stage being acknowledged (host_accepted, turn_submitted,
// or consumed -- the same closed set AttemptWake/Consume already use).
// The lease token itself is the bearer credential proving the caller
// legitimately holds this specific claim; ?as= is required for
// consistency with the rest of the surface (ADR 0045) but is not
// separately re-verified against the message here -- that check already
// happened at claim time.
func (s *Server) handleMessageAck(w http.ResponseWriter, r *http.Request, id string) {
	if s.DeliveryClaims == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "delivery claims not configured")
		return
	}
	if r.URL.Query().Get("as") == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "as query param required")
		return
	}
	var req messageAckRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxInputBytes+1)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Stage != delivery.StageHostAccepted && req.Stage != delivery.StageTurnSubmitted && req.Stage != delivery.StageConsumed {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			fmt.Sprintf("invalid stage %q; valid: host_accepted, turn_submitted, consumed", req.Stage))
		return
	}
	if !s.leaseMatchesMessage(r.Context(), w, id, req.Lease) {
		return
	}
	rd, attempt, err := s.DeliveryClaims.AckMessageDelivery(r.Context(), req.Lease, req.Stage)
	if err != nil {
		writeClaimError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"delivery": rd, "attempt": attempt})
}

type messageNackRequest struct {
	Lease              delivery.LeaseRef `json:"lease"`
	Retryable          bool              `json:"retryable"`
	Error              string            `json:"error"`
	NextAttemptSeconds int               `json:"next_attempt_seconds"`
}

// handleMessageNack services POST /messages/{id}/nack?as=<urn> (T07,
// messaging vNext): the caller declines or fails to finish processing a
// claimed delivery. Retryable schedules a retry after
// next_attempt_seconds (default: immediately claimable again);
// Retryable=false dead-letters it, matching AttemptWake's own Nack
// semantics (T06) and T03's legacy-import dead-lettering.
func (s *Server) handleMessageNack(w http.ResponseWriter, r *http.Request, id string) {
	if s.DeliveryClaims == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "delivery claims not configured")
		return
	}
	if r.URL.Query().Get("as") == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "as query param required")
		return
	}
	var req messageNackRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxInputBytes+1)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	if !s.leaseMatchesMessage(r.Context(), w, id, req.Lease) {
		return
	}
	rd, attempt, err := s.DeliveryClaims.NackMessageDelivery(r.Context(), req.Lease, req.Retryable, req.Error, time.Duration(req.NextAttemptSeconds)*time.Second)
	// Nack returns ErrDeadLettered as its own error value on the call that
	// SUCCESSFULLY dead-letters (T03's delivery_store.go hit this same
	// surprise first) -- it is the expected outcome of a non-retryable
	// Nack, not a failure.
	if err != nil && !errors.Is(err, delivery.ErrDeadLettered) {
		writeClaimError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"delivery": rd, "attempt": attempt})
}

// leaseMatchesMessage confirms the caller's presented lease actually
// belongs to the {id} named in the URL, not just some other delivery it
// happens to hold -- a caller-side consistency guard (the lease token
// itself is what actually authorizes Ack/Nack; this catches a caller's
// own mismatched request rather than adding a new security boundary).
// Writes an error response and returns false when they don't match.
func (s *Server) leaseMatchesMessage(ctx context.Context, w http.ResponseWriter, id string, lease delivery.LeaseRef) bool {
	deliveryID, ok, err := s.DeliveryClaims.DeliveryIDForMessage(ctx, id)
	if err != nil {
		writeClaimError(w, err)
		return false
	}
	if !ok || string(lease.DeliveryID) != deliveryID {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "lease does not correspond to message "+id)
		return false
	}
	return true
}

func writeClaimError(w http.ResponseWriter, err error) {
	switch {
	case isNotFound(err), errors.Is(err, delivery.ErrNotFound):
		// delivery.ErrNotFound is distinct from the messaging-level
		// sentinels isNotFound checks and previously fell through to the
		// generic 500 default below -- the same "silent 500 on an
		// otherwise-observable outcome" class this switch already patches
		// for ErrDeadlineExceeded/ErrNoDeliveryReady. Not currently
		// reachable through this handler's own callers (leaseMatchesMessage
		// already 400s an unrelated delivery id before Ack/Nack runs, and
		// go-messaging's sqlite Ack/Nack report a lease/attempt mismatch as
		// ErrStaleLease, not ErrNotFound) -- defensive completeness for the
		// sentinel's other callers (e.g. GetDelivery), not a demonstrated
		// live path today.
		writeError(w, http.StatusNotFound, CodeNotFound, "message not found")
	case isWrongRecipient(err):
		writeError(w, http.StatusConflict, CodeConflict, "caller is not the intended recipient")
	case errors.Is(err, store.ErrNoDeliveryTracking):
		writeError(w, http.StatusConflict, CodeConflict, err.Error())
	case errors.Is(err, delivery.ErrAlreadyClaimed):
		// The "stale lease" conformance case: a concurrent holder already
		// has an active claim. Observable, not a server error.
		writeError(w, http.StatusConflict, CodeConflict, "delivery already claimed by another holder")
	case errors.Is(err, delivery.ErrNoDeliveryReady):
		// Claim called before next_attempt_at -- a real, expected
		// "not yet" outcome (e.g. an impatient re-claim right after a
		// retryable Nack), not a server error.
		writeError(w, http.StatusConflict, CodeConflict, "no delivery ready to claim yet")
	case errors.Is(err, delivery.ErrDeadlineExceeded):
		// A distinct outcome from ErrDeadLettered (Nack's own success
		// signal for a deliberate non-retryable Nack, T03): here the
		// caller was trying to make progress (Claim/Ack) and the delivery
		// core dead-lettered the obligation out from under them because
		// its deadline had already passed -- the caller's own requested
		// operation did NOT succeed, so this is an observable conflict,
		// not a silent 200.
		writeError(w, http.StatusConflict, CodeConflict, "delivery deadline exceeded; the obligation has been dead-lettered")
	case errors.Is(err, delivery.ErrStaleLease):
		writeError(w, http.StatusConflict, CodeConflict, "lease is stale or superseded: "+err.Error())
	case errors.Is(err, delivery.ErrTerminalDelivery):
		writeError(w, http.StatusConflict, CodeConflict, "delivery is already terminal: "+err.Error())
	default:
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
	}
}

// POST /messages/request — blocking request/reply helper.
// Creates a request envelope and blocks until a matching response arrives
// or the timeout elapses (default 30s). Query params same as /broker/requests.
func (s *Server) handleMessageRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}

	q := r.URL.Query()
	var timeoutDur = defaultRequestTimeout
	if t := q.Get("timeout"); t != "" {
		d, err := time.ParseDuration(t)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid timeout: "+err.Error())
			return
		}
		timeoutDur = d
	}

	var env messaging.Envelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid body: "+err.Error())
		return
	}

	disp := messaging.NewDispatcher(s.MessageStore)
	waitCtx, cancel := context.WithTimeout(r.Context(), timeoutDur)
	defer cancel()

	resp, err := disp.Request(waitCtx, env)
	if err != nil {
		writeError(w, http.StatusGatewayTimeout, "timeout",
			"no response within "+timeoutDur.String())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleMessagesSubscribe services GET /messages/subscribe as an SSE stream.
//
// Query params:
//   - ?to=<urn>       — required; only envelopes addressed to this recipient
//   - ?kind=X,Y       — optional comma-separated Kind filter
//   - ?thread_id=T    — optional thread scope
//
// Each SSE event has event type "message" and data containing a
// JSON-encoded messaging.Envelope. A ": ping" comment is sent every 15s
// to keep proxies and load balancers alive.
//
// The connection streams envelopes created AFTER subscription time only
// (matching the messaging.Store.Subscribe contract — no historical replay;
// use GET /messages/inbox for that).
func (s *Server) handleMessagesSubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "streaming not supported")
		return
	}

	q := r.URL.Query()
	toURN := q.Get("to")
	if toURN == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "to query param required")
		return
	}
	to, err := messaging.ParseURN(toURN)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid to URN: "+err.Error())
		return
	}

	var f messaging.Filter
	if ks := q.Get("kind"); ks != "" {
		for _, k := range strings.Split(ks, ",") {
			f.Kind = append(f.Kind, messaging.Kind(strings.TrimSpace(k)))
		}
	}
	f.ThreadID = q.Get("thread_id")

	// Pass `to` into Subscribe so the Store filters at the source —
	// the post-hoc recipient check below is now redundant but kept as
	// a defense-in-depth safety net.
	ch, err := s.MessageStore.Subscribe(r.Context(), to, f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()

	for {
		select {
		case env, open := <-ch:
			if !open {
				return
			}
			// Store already filters by `to`; this is a defense-in-depth
			// check for any Store impl that doesn't enforce it.
			if !env.To.IsZero() && env.To != to {
				continue
			}
			b, err := json.Marshal(env)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func isNotFound(err error) bool {
	return errors.Is(err, messaging.ErrNotFound) ||
		strings.Contains(err.Error(), "no rows")
}

func isWrongRecipient(err error) bool {
	return errors.Is(err, store.ErrWrongRecipient)
}
