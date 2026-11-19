package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/ackfilter"
	"github.com/hollis-labs/tether/internal/messaging/envelope"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/seatstatus"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

// Typed messages and seat status (CW-20261114-0013, design v2 sections 5 and 8).
//
//	POST /messages/post            send a typed envelope (envelope.Envelope JSON); ?urgency=
//	POST /registry/handles         give a registry agent a @handle (operator)
//	GET  /messages/pending-replies inbound typed messages that wait on ?as=
//	POST /status                   declare a seat's status
//	GET  /status/{@handle}         the status card: declared state plus live facts

// SeatStore is the storage the typed-message and status routes need.
type SeatStore interface {
	GetSeatStatus(ctx context.Context, agentURN string) (seatstatus.Declared, bool, error)
	PutSeatStatus(ctx context.Context, d seatstatus.Declared) (seatstatus.Declared, bool, error)
	PendingReplies(ctx context.Context, agentURNs []string, handle string, limit int) ([]store.PendingReply, error)
	LatestWorkState(ctx context.Context, sessionID string) (store.EnvironmentWorkState, bool, error)
	SessionLastEventAt(ctx context.Context, sessionID string) (time.Time, error)
}

// KindSeatStatus is published when a seat's declared status changes.
const KindSeatStatus = "seat.status"

// KindSeatGateBlocked is published when a seat's Stop gate blocked a turn's end
// on pending typed messages (CW-20261114-0013).
const KindSeatGateBlocked = "seat.gate_blocked"

func (s *Server) registerTypedMessageRoutes(router *http.ServeMux) {
	if s.MessageStore != nil {
		router.HandleFunc("/messages/post", s.guardAs(s.handleMessagePost))
	}
	if s.Seats != nil {
		router.HandleFunc("/messages/pending-replies", s.guardAs(s.handlePendingReplies))
		router.HandleFunc("/messages/pending-replies/blocked", s.guardAs(s.handleStopGateBlocked))
		router.HandleFunc("/status", s.guardAs(s.handleStatusSet))
		router.HandleFunc("/status/", s.handleStatusGet)
	}
	if s.MessageStore != nil {
		router.HandleFunc("/operator-attestation/challenges/", s.handleOperatorAttestationChallenge)
	}
}

// handleDirectory resolves @handles for envelope.Check.
type handleDirectory struct {
	ctx context.Context
	s   *Server
}

func (d handleDirectory) Resolve(h string) (string, bool) { return d.s.resolveHandle(d.ctx, h) }

// resolveHandle maps @name to an address: the operator for @chrispian, else the
// registry agent whose handle (or, until backfilled, tether catalog id) is name,
// else the catalog agent's urn.
func (s *Server) resolveHandle(ctx context.Context, h string) (string, bool) {
	urn, ok := s.resolveHandleAddress(ctx, h)
	if !ok {
		return "", false
	}
	// A handle held by an alias names the canonical address (address_aliases.go).
	return s.canonicalAlias(ctx, urn), true
}

func (s *Server) resolveHandleAddress(ctx context.Context, h string) (string, bool) {
	if !envelope.ValidHandle(h) {
		return "", false
	}
	if h == envelope.Operator {
		return identity.OperatorID, true
	}
	name := strings.TrimPrefix(h, "@")
	if name == registry.ReservedHandle {
		return "", false // only the operator answers to it (handled above)
	}
	if s.Registry != nil {
		for _, substrate := range []string{registry.HandleSubstrate, "tether"} {
			if p, err := s.Registry.LookupBy(ctx, registry.KindAgent, name, substrate); err == nil && p.URN != "" {
				return p.URN, true
			}
		}
	}
	if s.Catalog != nil {
		if cat, err := s.Catalog.Load(); err == nil && cat != nil {
			for id, a := range cat.Agents {
				if a.Handle == name && a.Handle != registry.ReservedHandle {
					if a.URN != "" {
						return a.URN, true
					}
					return "msg://agent/agent-mux/" + id, true
				}
			}
			if a, ok := cat.Agents[name]; ok {
				if a.URN != "" {
					return a.URN, true
				}
				return "msg://agent/agent-mux/" + name, true
			}
		}
	}
	return "", false
}

// handleFor is the @handle of an address, or "" when it has none. An aliased
// address answers to the handle of any of its forms, the canonical one first;
// an agent address never answers to Chrispian's (reachableForms).
func (s *Server) handleFor(ctx context.Context, urn string) string {
	for _, form := range s.reachableForms(ctx, urn) {
		if h := s.handleForAddress(ctx, form); h != "" {
			return h
		}
	}
	return ""
}

func (s *Server) handleForAddress(ctx context.Context, urn string) string {
	if urn == identity.OperatorID {
		return envelope.Operator
	}
	if s.Registry != nil {
		if ids, err := s.Registry.LookupExternalIDsForURN(ctx, urn); err == nil {
			for _, substrate := range []string{registry.HandleSubstrate, "tether"} {
				for _, id := range ids {
					if id.Substrate == substrate && envelope.ValidHandle("@"+id.ExternalID) && id.ExternalID != registry.ReservedHandle {
						return "@" + id.ExternalID
					}
				}
			}
		}
	}
	if s.Catalog != nil {
		if cat, err := s.Catalog.Load(); err == nil && cat != nil {
			for id, a := range cat.Agents {
				if a.URN == urn || "msg://agent/agent-mux/"+id == urn {
					if a.Handle != "" && a.Handle != registry.ReservedHandle {
						return "@" + a.Handle
					}
					if id == registry.ReservedHandle {
						return ""
					}
					return "@" + id
				}
			}
		}
	}
	return ""
}

func writeEnvelopeError(w http.ResponseWriter, err error) {
	var e *envelope.Error
	if errors.As(err, &e) {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: ErrorDetail{Code: CodeInvalidRequest, Message: e.Error(), Details: map[string]string{"field": e.Field, "reason": e.Reason}}})
		return
	}
	var st *seatstatus.Error
	if errors.As(err, &st) {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: ErrorDetail{Code: CodeInvalidRequest, Message: st.Error(), Details: map[string]string{"field": st.Field, "reason": st.Reason}}})
		return
	}
	writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
}

// MessagePostResponse is the POST /messages/post body.
type MessagePostResponse struct {
	IDs      []string          `json:"ids"`
	ThreadID string            `json:"thread_id"`
	Envelope envelope.Envelope `json:"envelope"`
}

// POST /messages/post?as=<sender>. The body is an envelope; from is never read
// from it: the sender is the verified principal (or ?as=, checked like any send).
func (s *Server) handleMessagePost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "body: "+err.Error())
		return
	}
	e, err := envelope.Decode(raw)
	if err != nil {
		writeEnvelopeError(w, err)
		return
	}
	if e.From != "" {
		writeEnvelopeError(w, &envelope.Error{Field: "from", Reason: "is stamped by the server; omit it"})
		return
	}
	stub := messaging.Envelope{}
	if as := r.URL.Query().Get("as"); as != "" {
		if stub.From, err = messaging.ParseURN(as); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "as: "+err.Error())
			return
		}
	} else if addr, ok := s.seatAddressOfPrincipal(r); ok {
		// A worker session's credential lists its session address first; a typed
		// message is from the seat, so stamp the first address that has a handle.
		stub.From = addr
	}
	if !s.applySendIdempotencyKey(w, r, &stub.Metadata) {
		return
	}
	// Urgency is message metadata, not an envelope field: the lifecycle events
	// already carry it, and a critical approval is an approval_request at high.
	if u := r.URL.Query().Get("urgency"); u != "" {
		switch u {
		case "very-low", "low", "normal", "high":
		default:
			writeEnvelopeError(w, &envelope.Error{Field: "urgency", Reason: "must be very-low, low, normal or high"})
			return
		}
		if stub.Metadata == nil {
			stub.Metadata = map[string]string{}
		}
		stub.Metadata["urgency"] = u
	}
	if !s.stampSender(w, r, &stub) {
		return
	}
	if stub.From.IsZero() {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "cannot tell who is sending: authenticate, or pass ?as=<your address>")
		return
	}
	ctx := r.Context()
	from := canonicalAddress(stub.From)
	e.Normalize()
	e.From = s.handleFor(ctx, from)
	if e.From == "" {
		e.From = from
	}

	var parent *envelope.Parent
	threadID := ""
	if e.InReplyTo != "" {
		p, err := s.MessageStore.Get(ctx, e.InReplyTo)
		// The sender may know itself by another address than the parent used (the
		// agent-mux binding target vs its catalog urn): any of the seat's addresses counts.
		mine := s.seatAddresses(ctx, from, e.From)
		if err != nil || (!slices.Contains(mine, canonicalAddress(p.From)) && !slices.Contains(mine, canonicalAddress(p.To))) {
			writeEnvelopeError(w, &envelope.Error{Field: "in_reply_to", Reason: "no such message among yours"})
			return
		}
		threadID = p.ThreadID
		if threadID == "" {
			threadID = p.ID
		}
		parent = &envelope.Parent{}
		if p.ContentType == envelope.ContentType {
			var pe envelope.Envelope
			if json.Unmarshal(p.Payload, &pe) == nil {
				parent.Envelope = &pe
			}
		}
		// A reply goes back to whoever sent the parent and awaits them
		// (CW-20261117-0003): to and awaiting default to the parent's sender, so
		// the asker is woken (triage awaiting_recipient / reply_to_own).
		if asker := s.replyAsker(ctx, p, from); asker != "" {
			if len(e.To) == 0 {
				e.To = []string{asker}
			}
			if e.Awaiting == "" {
				e.Awaiting = asker
			}
		}
	}
	if strings.HasPrefix(e.Re, "thread:") && threadID == "" {
		threadID = strings.TrimPrefix(e.Re, "thread:")
	}
	urns, err := envelope.Check(e, handleDirectory{ctx, s}, parent)
	if err != nil {
		writeEnvelopeError(w, err)
		return
	}
	if s.refusesAck(e, parent) {
		writeEnvelopeError(w, &envelope.Error{Field: "body", Reason: ackfilter.Refusal})
		return
	}
	body, err := json.Marshal(e)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	resp := MessagePostResponse{Envelope: e, ThreadID: threadID}
	for i, urn := range urns {
		to, err := messaging.ParseURN(urn)
		if err != nil {
			writeEnvelopeError(w, &envelope.Error{Field: fmt.Sprintf("to[%d]", i), Reason: "resolves to an invalid address " + urn})
			return
		}
		meta := map[string]string{}
		for k, v := range stub.Metadata {
			meta[k] = v
		}
		if key := meta["tether.message_id"]; key != "" && len(urns) > 1 {
			meta["tether.message_id"] = fmt.Sprintf("%s:%d", key, i)
		}
		scopeIdempotencyKey(meta)
		msg := messaging.Envelope{From: stub.From, To: to, Kind: envelope.TransportKind(e.Kind), ThreadID: resp.ThreadID, InReplyTo: e.InReplyTo,
			Payload: body, ContentType: envelope.ContentType, Metadata: meta}
		sent, err := s.sendMessage(ctx, msg)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, ErrRecipientDeprecated) {
				status = http.StatusConflict
			}
			writeError(w, status, CodeInternalError, fmt.Sprintf("sending to %s: %v (sent so far: %v)", e.To[i], err, resp.IDs))
			return
		}
		resp.IDs = append(resp.IDs, sent.ID)
		if resp.ThreadID == "" {
			resp.ThreadID = sent.ThreadID
			if resp.ThreadID == "" {
				resp.ThreadID = sent.ID
			}
		}
	}
	writeJSON(w, http.StatusCreated, resp)
}

// refusesAck reports whether e is a pure acknowledgement reply the hub refuses
// (CW-20261118-0050): a reply of kind update or message that carries no answer,
// status, evidence or options, whose body is acknowledgement words only, to a
// parent that is no ask of the replier. The catalog switch
// defaults.mail_triage.ack_filter: false turns it off.
func (s *Server) refusesAck(e envelope.Envelope, parent *envelope.Parent) bool {
	if e.InReplyTo == "" || (e.Kind != envelope.KindUpdate && e.Kind != envelope.KindMessage) {
		return false
	}
	if e.Answer != nil || e.Status != "" || len(e.Evidence) > 0 || len(e.Options) > 0 || e.Deliverable != "" {
		return false
	}
	if !ackfilter.IsAck(e.Body) {
		return false
	}
	if parent == nil || parent.Envelope == nil {
		return false // untyped or unknown parent: let it through
	}
	pe := parent.Envelope
	switch pe.Kind {
	case envelope.KindUpdate, envelope.KindMessage:
	default:
		return false // the parent asked something: an ack may be the answer
	}
	// An answer to a free-text ask is never refused: the parent asks (a '?'),
	// awaits anyone, or is addressed to the replier with an awaiting.
	if strings.Contains(pe.Body, "?") || strings.Contains(pe.Subject, "?") || strings.TrimSpace(pe.Awaiting) != "" {
		return false
	}
	if s.Catalog != nil {
		if cat, err := s.Catalog.Load(); err == nil && cat != nil && !cat.Global.AckFilterEnabled() {
			return false
		}
	}
	return true
}

// replyAsker is the @handle of the sender of p, the message a reply from
// replier answers: "" when it has no valid handle or is the replier itself (a
// follow-up to one's own message).
func (s *Server) replyAsker(ctx context.Context, p messaging.Envelope, replier string) string {
	sender := canonicalAddress(p.From)
	if sender == replier {
		return ""
	}
	h := s.handleFor(ctx, sender)
	if !envelope.ValidHandle(h) || h == s.handleFor(ctx, replier) {
		return ""
	}
	return h
}

// PendingRepliesResponse is the GET /messages/pending-replies body.
type PendingRepliesResponse struct {
	Handle  string               `json:"handle"`
	Pending []store.PendingReply `json:"pending"`
}

// GET /messages/pending-replies?as=<agent>: the Stop gate's question.
func (s *Server) handlePendingReplies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	as := stampedActor(r, r.URL.Query().Get("as"))
	if as == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "as is required")
		return
	}
	// The same canonical form handleMessagePost stores as the sender.
	if addr, err := messaging.ParseURN(as); err == nil {
		as = canonicalAddress(addr)
	}
	handle := s.handleFor(r.Context(), as)
	pending, err := s.Seats.PendingReplies(r.Context(), s.seatAddresses(r.Context(), as, handle), handle, 20)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, PendingRepliesResponse{Handle: handle, Pending: pending})
}

// StatusSetRequest is the POST /status body.
type StatusSetRequest struct {
	Target     string           `json:"target,omitempty"` // @handle; default the caller
	State      seatstatus.State `json:"state"`
	Reason     string           `json:"reason,omitempty"`
	Ref        string           `json:"ref,omitempty"`
	Owner      string           `json:"owner,omitempty"`
	TTLSeconds int              `json:"ttl_seconds,omitempty"`
	ExpiresAt  *time.Time       `json:"expires_at,omitempty"`
}

// StatusSetResponse is the POST /status body: the stored declaration and
// whether it changed what the seat declares.
type StatusSetResponse struct {
	Handle   string              `json:"handle"`
	Declared seatstatus.Declared `json:"declared"`
	Changed  bool                `json:"changed"`
}

// POST /status?as=<caller>. A seat sets its own status; the operator and a
// catalog manager may set another seat's.
func (s *Server) handleStatusSet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	var req StatusSetRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid body: "+err.Error())
		return
	}
	ctx := r.Context()
	caller := stampedActor(r, r.URL.Query().Get("as"))
	if caller == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "cannot tell who is setting: authenticate, or pass ?as=<your address>")
		return
	}
	verification, principal := s.claimVerification(r, caller)
	// A seat's own live session is the seat (#568 regression): its session
	// credential, not a claim, binds it to the seat's logical agent. Without
	// ?as= it sets the seat's status, not the session URN's; with ?as= naming
	// that seat (any of its forms) the claim verifies.
	if seat, ok := s.sessionSeat(ctx, r); ok {
		switch as := r.URL.Query().Get("as"); {
		case as == "":
			caller, verification = seat, "stamped"
		case slices.Contains(s.seatForms(ctx, seat), s.canonicalSeat(ctx, canonicalClaim(as))):
			caller, verification = seat, "verified"
		}
	}
	target := caller
	if req.Target == "@self" {
		req.Target = ""
	}
	if req.Target != "" {
		urn, ok := s.resolveHandle(ctx, req.Target)
		if !ok {
			writeEnvelopeError(w, &seatstatus.Error{Field: "target", Reason: "unknown handle " + req.Target})
			return
		}
		target = urn
	}
	if !s.maySetStatus(ctx, r, caller, target, verification) {
		writeError(w, http.StatusForbidden, CodeForbidden, "only the seat itself, the operator or a verified manager may set a seat's status")
		return
	}
	in := seatstatus.Input{State: req.State, Reason: req.Reason, Ref: req.Ref, Owner: req.Owner, TTL: time.Duration(req.TTLSeconds) * time.Second}
	if req.ExpiresAt != nil {
		in.ExpiresAt = *req.ExpiresAt
	}
	now := time.Now().UTC()
	expires, err := in.Validate(now)
	if err != nil {
		writeEnvelopeError(w, err)
		return
	}
	if in.Owner != "" {
		if _, ok := s.resolveHandle(ctx, in.Owner); !ok {
			writeEnvelopeError(w, &seatstatus.Error{Field: "owner", Reason: "unknown handle " + in.Owner})
			return
		}
	}
	d := seatstatus.Declared{AgentURN: target, State: in.State, Reason: in.Reason, Ref: in.Ref, Owner: in.Owner, Since: now, ExpiresAt: expires, SetBy: caller,
		SetByVerification: verification, SetByPrincipal: principal, UpdatedAt: now}
	stored, changed, err := s.Seats.PutSeatStatus(ctx, d)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	handle := s.handleFor(ctx, target)
	if changed && s.Bus != nil {
		payload := map[string]any{"agent_urn": target, "handle": handle, "state": stored.State, "set_by": caller}
		for k, v := range map[string]string{"reason": stored.Reason, "ref": stored.Ref, "owner": stored.Owner,
			"set_by_verification": stored.SetByVerification, "set_by_principal": stored.SetByPrincipal} {
			if v != "" {
				payload[k] = v
			}
		}
		if !stored.ExpiresAt.IsZero() {
			payload["expires_at"] = stored.ExpiresAt.Format(time.RFC3339)
		}
		raw, _ := json.Marshal(payload)
		e := events.Event{Scope: events.ScopeDaemon, Kind: KindSeatStatus, At: now, PayloadJSON: string(raw)}
		e.Envelope.Dims.AgentURN = target
		e.Envelope.Class.Category = "state"
		// Setting status never fails on the event (it is not a gate).
		_ = s.Bus.Publish(context.WithoutCancel(ctx), e)
	}
	writeJSON(w, http.StatusOK, StatusSetResponse{Handle: handle, Declared: stored, Changed: changed})
}

// maySetStatus decides who may set target's status (CW-20261114-0130):
//   - the operator credential, any seat;
//   - a caller the hub verified (or stamped from its own address), its own seat, or any seat when its
//     catalog agent is a manager;
//   - without a verified principal, only with identity off on the local socket, as before: the seat
//     itself, the local operator, or a catalog manager.
//
// A claim that did not verify sets nothing, in every identity mode: it would otherwise let anyone that
// names a seat (or a manager) set that seat's status.
func (s *Server) maySetStatus(ctx context.Context, r *http.Request, caller, target, verification string) bool {
	if p, ok := verifiedPrincipal(r); ok && identity.IsOperatorCredential(p) {
		return true
	}
	switch verification {
	case "verified", "stamped":
		if s.actsFor(ctx, caller, target) {
			return true
		}
		a, ok := s.catalogAgentFor(ctx, caller)
		return ok && a.IsManager()
	case "":
		if (s.IdentityMode != "" && s.IdentityMode != identity.Off) || identity.IsRemote(ctx) {
			return false
		}
		if s.actsFor(ctx, caller, target) || s.canonicalAlias(ctx, caller) == s.canonicalAlias(ctx, identity.OperatorID) {
			return true
		}
		a, ok := s.catalogAgentFor(ctx, caller)
		return ok && a.IsManager()
	}
	return false
}

// sessionSeat is the seat whose own live session the request's verified
// principal is: a session credential (verified by its token, never by a
// claimed address) for a non-terminal session bound to a logical agent. It
// returns the seat's canonical address.
func (s *Server) sessionSeat(ctx context.Context, r *http.Request) (string, bool) {
	p, ok := verifiedPrincipal(r)
	if !ok || p.Kind != "session" || p.SessionID == "" || s.Service == nil {
		return "", false
	}
	row, err := s.Service.GetSession(p.SessionID)
	if err != nil || row == nil || row.LogicalAgentID == "" || session.State(row.State).Terminal() {
		return "", false
	}
	return s.canonicalSeat(ctx, logicalAgentPrefix+row.LogicalAgentID), true
}

// catalogAgentFor is the catalog agent whose identity is urn: by its tether
// catalog id in the registry, else by its urn: in the catalog. Never by
// handle: a handle can be given to anyone, an identity cannot.
func (s *Server) catalogAgentFor(ctx context.Context, urn string) (config.Agent, bool) {
	if s.Catalog == nil {
		return config.Agent{}, false
	}
	cat, err := s.Catalog.Load()
	if err != nil || cat == nil {
		return config.Agent{}, false
	}
	if s.Registry != nil {
		if ids, err := s.Registry.LookupExternalIDsForURN(ctx, urn); err == nil {
			for _, id := range ids {
				if id.Substrate == "tether" {
					if a, ok := cat.Agents[id.ExternalID]; ok {
						return a, true
					}
				}
			}
		}
	}
	for _, a := range cat.Agents {
		if a.URN != "" && a.URN == urn {
			return a, true
		}
	}
	return config.Agent{}, false
}

// StatusCard is the GET /status/{@handle} body: what the seat declares beside
// what the hub observes. Effective is the declared state unless it expired
// (then "unknown").
type StatusCard struct {
	Handle    string               `json:"handle"`
	AgentURN  string               `json:"agent_urn"`
	Declared  *seatstatus.Declared `json:"declared,omitempty"`
	Expired   bool                 `json:"expired,omitempty"`
	Effective string               `json:"effective"`
	Live      StatusLive           `json:"live"`
}

// StatusLive are the observed facts.
type StatusLive struct {
	SessionID      string     `json:"session_id,omitempty"`
	Online         bool       `json:"online"`
	Health         string     `json:"health,omitempty"`
	WorkState      string     `json:"work_state,omitempty"`
	WorkStateSince *time.Time `json:"work_state_since,omitempty"`
	LastActivity   *time.Time `json:"last_activity,omitempty"`
	UnreadMail     int        `json:"unread_mail"`
	PendingReplies int        `json:"pending_replies"`
}

// GET /status/{@handle}
func (s *Server) handleStatusGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	ctx := r.Context()
	h := strings.TrimPrefix(r.URL.Path, "/status/")
	if !strings.HasPrefix(h, "@") {
		h = "@" + h
	}
	urn, ok := s.resolveHandle(ctx, h)
	if !ok {
		writeError(w, http.StatusNotFound, CodeNotFound, "unknown handle "+h)
		return
	}
	card, err := s.statusCard(ctx, h, urn)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, card)
}

func (s *Server) statusCard(ctx context.Context, h, urn string) (StatusCard, error) {
	now := time.Now().UTC()
	card := StatusCard{Handle: h, AgentURN: urn, Effective: "unknown"}
	d, found, err := s.Seats.GetSeatStatus(ctx, urn)
	if err != nil {
		return card, err
	}
	if found {
		card.Declared = &d
		card.Expired = d.Expired(now)
		if !card.Expired {
			card.Effective = string(d.State)
		}
	}
	if addr, err := messaging.ParseURN(urn); err == nil && addr.Kind == messaging.KindAgent && s.Service != nil {
		if sid, err := s.Service.ResolveActorSession(ctx, addr.ID); err == nil && sid != "" {
			card.Live.SessionID = sid
			if rh, ok := s.Service.RuntimeHealth(sid); ok {
				card.Live.Online = true
				card.Live.Health = rh.Health.State.String()
			}
			if ws, ok, err := s.Seats.LatestWorkState(ctx, sid); err == nil && ok {
				card.Live.WorkState = ws.Value
				if !ws.At.IsZero() {
					at := ws.At
					card.Live.WorkStateSince = &at
				}
			}
			if at, err := s.Seats.SessionLastEventAt(ctx, sid); err == nil && !at.IsZero() {
				card.Live.LastActivity = &at
			}
		}
	}
	if addr, err := messaging.ParseURN(urn); err == nil {
		if n, err := s.unreadCount(ctx, addr); err == nil {
			card.Live.UnreadMail = n
		}
	}
	if pending, err := s.Seats.PendingReplies(ctx, s.seatAddresses(ctx, urn, h), h, 50); err == nil {
		card.Live.PendingReplies = len(pending)
	}
	if !found && card.Live.Online {
		card.Effective = "undeclared"
	}
	return card, nil
}

// catalogHandles maps catalog agent id to its handle: override.
func (s *Server) catalogHandles() map[string]string {
	out := map[string]string{}
	if s.Catalog == nil {
		return out
	}
	cat, err := s.Catalog.Load()
	if err != nil || cat == nil {
		return out
	}
	for id, a := range cat.Agents {
		if a.Handle != "" {
			out[id] = a.Handle
		}
	}
	return out
}

// HandleSetRequest is the POST /registry/handles body.
type HandleSetRequest struct {
	URN    string `json:"urn"`
	Handle string `json:"handle"` // @name or name
}

// POST /registry/handles: give an existing registry agent a @handle (for seats
// that are not catalog agents, such as local agent-boot sessions). Operator
// only; a handle already held by another agent is refused.
func (s *Server) handleRegistryHandleSet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	if p, ok := verifiedPrincipal(r); ok && !identity.IsOperatorCredential(p) {
		writeError(w, http.StatusForbidden, CodeForbidden, "only the operator may assign handles")
		return
	}
	var req HandleSetRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid body: "+err.Error())
		return
	}
	h := "@" + strings.TrimPrefix(strings.TrimSpace(req.Handle), "@")
	if !envelope.ValidHandle(h) || h == envelope.Operator {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "handle must be @name: lowercase letters, digits, '-' or '_', starting with a letter (and not the reserved "+envelope.Operator+")")
		return
	}
	if req.URN == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "urn is required")
		return
	}
	if err := s.Registry.AttachExternalID(r.Context(), req.URN, registry.HandleSubstrate, strings.TrimPrefix(h, "@")); err != nil {
		if errors.Is(err, registry.ErrInvalidRequest) {
			writeError(w, http.StatusConflict, CodeConflict, err.Error())
			return
		}
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"urn": req.URN, "handle": h})
}

// seatAddresses is every address a seat is known by: the one asked about, the
// one its handle resolves to (the catalog/registry URN mail is sent to), and
// its msg://agent/agent-mux/<catalog id> binding target (what a worker session
// knows itself as, TETHER_AGENT_URN). Pending replies match on all of them.
func (s *Server) seatAddresses(ctx context.Context, urn, handle string) []string {
	out := []string{urn}
	add := func(u string) {
		for _, x := range out {
			if x == u {
				return
			}
		}
		out = append(out, u)
	}
	if handle != "" {
		if u, ok := s.resolveHandle(ctx, handle); ok {
			add(u)
		}
	}
	if s.Catalog != nil {
		if cat, err := s.Catalog.Load(); err == nil && cat != nil {
			for id, a := range cat.Agents {
				target := "msg://agent/agent-mux/" + id
				if a.URN == urn || target == urn || (a.URN != "" && slices.Contains(out, a.URN)) {
					add(target)
					if a.URN != "" {
						add(a.URN)
					}
				}
			}
		}
	}
	// Mail stored under any form of an aliased address is the seat's too, but
	// never Chrispian's mail through an agent alias (reachableForms).
	for _, u := range slices.Clone(out) {
		for _, form := range s.reachableForms(ctx, u) {
			add(form)
		}
	}
	return out
}

// seatAddressOfPrincipal is the address a typed post from this principal is
// from: the first of its verified addresses that names a seat with a @handle,
// mapped to that seat's catalog urn: when it has one (seats bind and receive on
// it; mail to the msg://agent/agent-mux/<id> form is accepted and dropped), else
// the address itself. stampSender then verifies it like any claim.
func (s *Server) seatAddressOfPrincipal(r *http.Request) (messaging.Address, bool) {
	p, ok := verifiedPrincipal(r)
	if !ok {
		return messaging.Address{}, false
	}
	candidates := append([]string{p.DefaultAddress()}, p.Addresses...)
	for _, a := range candidates {
		if s.handleFor(r.Context(), a) == "" {
			continue
		}
		if urn := s.catalogURNFor(a); urn != "" && slices.Contains(candidates, urn) {
			a = urn
		}
		if addr, err := messaging.ParseURN(a); err == nil {
			return addr, true
		}
	}
	return messaging.Address{}, false
}

// catalogURNFor is the catalog urn: of the agent addr names (by that urn or by
// its msg://agent/agent-mux/<id> form), or "".
func (s *Server) catalogURNFor(addr string) string {
	if s.Catalog == nil {
		return ""
	}
	cat, err := s.Catalog.Load()
	if err != nil || cat == nil {
		return ""
	}
	for id, a := range cat.Agents {
		if a.URN != "" && (a.URN == addr || "msg://agent/agent-mux/"+id == addr) {
			return a.URN
		}
	}
	return ""
}

// StopGateBlockedRequest is the POST /messages/pending-replies/blocked body.
type StopGateBlockedRequest struct {
	MessageIDs []string `json:"message_ids"`
}

// POST /messages/pending-replies/blocked?as=<seat>: the Stop gate reports that
// it blocked a turn on these messages; the hub publishes seat.gate_blocked. An
// observation only: it changes no message or delivery state.
func (s *Server) handleStopGateBlocked(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	var req StopGateBlockedRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid body: "+err.Error())
		return
	}
	if len(req.MessageIDs) == 0 || len(req.MessageIDs) > 20 {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "message_ids: 1 to 20 ids")
		return
	}
	for _, id := range req.MessageIDs {
		if id == "" || len(id) > 128 || strings.ContainsAny(id, " \r\n") {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "message_ids: each a message id")
			return
		}
	}
	as := stampedActor(r, r.URL.Query().Get("as"))
	if as == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "as is required")
		return
	}
	ctx := r.Context()
	seat := s.canonicalSeat(ctx, as)
	handle := s.handleFor(ctx, seat)
	if s.Bus != nil {
		raw, _ := json.Marshal(map[string]any{"agent_urn": seat, "handle": handle, "message_ids": req.MessageIDs, "count": len(req.MessageIDs)})
		e := events.Event{Scope: events.ScopeDaemon, Kind: KindSeatGateBlocked, At: time.Now().UTC(), PayloadJSON: string(raw)}
		if p, ok := verifiedPrincipal(r); ok && p.SessionID != "" {
			e.Scope, e.SessionID = events.ScopeSession, p.SessionID
		}
		e.Envelope.Dims.AgentURN = seat
		e.Envelope.Class.Category = "state"
		if err := s.Bus.Publish(context.WithoutCancel(ctx), e); err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"recorded": len(req.MessageIDs), "handle": handle})
}
