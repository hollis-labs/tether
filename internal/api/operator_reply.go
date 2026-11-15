package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/envelope"
	"github.com/hollis-labs/tether/internal/operatorattest"
	"github.com/hollis-labs/tether/internal/store"
)

// Operator-attested replies (CW-20261114-0152, docs/operator-attested-replies.md):
// Chrispian answers a typed card from Tachyon, and Tether stores it as his
// verified reply because Tangent proves he stepped up and chose that option of
// that card. Three routes:
//
//	POST /messages/{id}/operator-reply/challenge           Tachyon: a single-use challenge
//	GET  /operator-attestation/challenges/{challenge_id}   Tangent: the canonical card it shows and signs
//	POST /messages/{id}/operator-reply                     Tachyon: the reply with Tangent's assertion

// OperatorReplyStore is the store behind the routes (*store.Store).
type OperatorReplyStore interface {
	CreateOperatorReplyChallenge(ctx context.Context, c store.OperatorReplyChallenge, limit int) error
	OperatorReplyChallenge(ctx context.Context, id string) (store.OperatorReplyChallenge, error)
	ConsumeOperatorReplyChallenge(ctx context.Context, id string, now time.Time) (bool, error)
	OperatorReplyProofUsed(ctx context.Context, jti, elevationID string) (bool, error)
	OperatorAnswered(ctx context.Context, messageID string, operatorForms []string) (bool, error)
	ReserveOperatorReply(ctx context.Context, r store.OperatorReplyReservation, operatorForms []string) error
	CompleteOperatorReply(ctx context.Context, messageID, replyID string) error
	ReleaseOperatorReply(ctx context.Context, messageID string) error
}

// Contract error codes.
const (
	CodeFeatureDisabled     = "feature_disabled"
	CodeNotOperatorDevice   = "not_operator_device"
	CodeScopeRequired       = "scope_required"
	CodeParentNotFound      = "parent_not_found"
	CodeParentNotOperators  = "parent_not_operators"
	CodeNotAnswerable       = "not_answerable"
	CodeInvalidOption       = "invalid_option"
	CodeAlreadyAnswered     = "already_answered"
	CodeChallengeUnknown    = "challenge_unknown"
	CodeChallengeExpired    = "challenge_expired"
	CodeAssertionInvalid    = "assertion_invalid"
	CodeAssertionStale      = "assertion_stale"
	CodeAssertionUnbound    = "assertion_unbound"
	CodeReplayed            = "replayed"
	CodeTooManyChallenges   = "too_many_challenges"
	maxOutstandingChallenge = 10
	operatorReplyExcerpt    = 500
)

// Audit events.
const (
	AuditOperatorReplyAttested = "operator_reply_attested"
	AuditOperatorReplyRefused  = "operator_reply_refused"
)

// MetaOperatorAttestation is the stored reply's proof record. Reserved: set
// only here (storeAttestedReply); every ingress drops a client's value.
const MetaOperatorAttestation = store.OperatorAttestationKey

type operatorReplyError struct {
	status  int
	code    string
	message string
	details map[string]string
}

func (e *operatorReplyError) Error() string { return e.code + ": " + e.message }

func orErr(status int, code, format string, a ...any) *operatorReplyError {
	return &operatorReplyError{status: status, code: code, message: fmt.Sprintf(format, a...)}
}

func writeOperatorReplyError(w http.ResponseWriter, e *operatorReplyError) {
	writeJSON(w, e.status, ErrorResponse{Error: ErrorDetail{Code: e.code, Message: e.message, Details: e.details}})
}

// operatorReplyConfig is the trust root in force, or feature_disabled.
func (s *Server) operatorReplyConfig() (*operatorattest.Config, *operatorReplyError) {
	if s.OperatorAttestation == nil || s.OperatorReplies == nil {
		return nil, orErr(http.StatusNotFound, CodeFeatureDisabled, "operator-attested replies are off: no approved operator_attestation in the catalog")
	}
	c := s.OperatorAttestation()
	if c == nil {
		return nil, orErr(http.StatusNotFound, CodeFeatureDisabled, "operator-attested replies are off: no approved operator_attestation in the catalog")
	}
	return c, nil
}

// operatorDevice is the caller when it is a verified, operator-paired device
// (not an issued token) holding scope.
func operatorDevice(r *http.Request, scope string) (identity.Principal, *operatorReplyError) {
	p, ok := verifiedPrincipal(r)
	if !ok || p.Kind != "device" || p.TokenID != "" || p.CreatedBy != identity.OperatorID || p.ID == identity.OperatorID {
		return identity.Principal{}, orErr(http.StatusForbidden, CodeNotOperatorDevice, "a verified device the operator paired (not an issued token) is required")
	}
	if !identity.HasDeviceScope(p, scope) {
		return identity.Principal{}, orErr(http.StatusForbidden, CodeScopeRequired, "required scope: %s", scope)
	}
	return p, nil
}

// answerableParent loads id as a typed card addressed to the operator.
func (s *Server) answerableParent(ctx context.Context, id string) (messaging.Envelope, envelope.Envelope, *operatorReplyError) {
	p, err := s.MessageStore.Get(ctx, id)
	if err != nil {
		return p, envelope.Envelope{}, orErr(http.StatusNotFound, CodeParentNotFound, "no message %s", id)
	}
	var pe envelope.Envelope
	if p.ContentType != envelope.ContentType || json.Unmarshal(p.Payload, &pe) != nil || !answerableKind(pe.Kind) {
		e := orErr(http.StatusBadRequest, CodeNotAnswerable, "message %s is not a question, decision or approval_request", id)
		e.details = map[string]string{"field": "in_reply_to", "reason": "not a question, decision or approval_request"}
		return p, pe, e
	}
	forms := s.operatorForms(ctx)
	if to := p.To.URN(); !slices.Contains(forms, to) && !slices.Contains(forms, s.canonicalAlias(ctx, to)) {
		return p, pe, orErr(http.StatusForbidden, CodeParentNotOperators, "message %s is not addressed to the operator", id)
	}
	return p, pe, nil
}

func answerableKind(k envelope.Kind) bool {
	return k == envelope.KindQuestion || k == envelope.KindDecision || k == envelope.KindApprovalRequest
}

// operatorForms is every address of the operator, canonical first (#554).
func (s *Server) operatorForms(ctx context.Context) []string {
	forms := s.aliasForms(ctx, identity.OperatorID)
	if !slices.Contains(forms, identity.OperatorID) {
		forms = append(forms, identity.OperatorID)
	}
	return forms
}

func checkOption(pe envelope.Envelope, option string) *operatorReplyError {
	if pe.OptionIDs()[option] {
		return nil
	}
	ids := make([]string, 0)
	for id := range pe.OptionIDs() {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	e := orErr(http.StatusBadRequest, CodeInvalidOption, "option %q is not one of %v", option, ids)
	e.details = map[string]string{"field": "answer.option", "reason": fmt.Sprintf("must be one of %s", strings.Join(ids, ", "))}
	return e
}

// handleOperatorReplyChallenge serves POST /messages/{id}/operator-reply/challenge.
func (s *Server) handleOperatorReplyChallenge(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	cfg, oe := s.operatorReplyConfig()
	if oe != nil {
		writeOperatorReplyError(w, oe)
		return
	}
	p, oe := operatorDevice(r, identity.ScopeOperatorReply)
	if oe != nil {
		writeOperatorReplyError(w, oe)
		return
	}
	var req struct {
		Option string `json:"option"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid body: "+err.Error())
		return
	}
	ctx := r.Context()
	_, pe, oe := s.answerableParent(ctx, id)
	if oe == nil {
		oe = checkOption(pe, req.Option)
	}
	if oe == nil {
		if answered, err := s.OperatorReplies.OperatorAnswered(ctx, id, s.operatorForms(ctx)); err != nil {
			oe = orErr(http.StatusInternalServerError, CodeInternalError, "%v", err)
		} else if answered {
			oe = orErr(http.StatusConflict, CodeAlreadyAnswered, "the operator already answered %s", id)
		}
	}
	if oe != nil {
		writeOperatorReplyError(w, oe)
		return
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	now := time.Now().UTC()
	c := store.OperatorReplyChallenge{ID: "orc_" + uuid.NewString(), MessageID: id, Option: req.Option,
		Binding: operatorattest.Binding(id, req.Option), BodySHA256: operatorattest.SHA256Hex(pe.Body), DeviceID: p.ID,
		CreatedAt: now, ExpiresAt: now.Add(cfg.MaxAge)}
	encoded := base64.RawURLEncoding.EncodeToString(nonce)
	c.NonceSHA256 = operatorattest.SHA256Hex(encoded)
	if err := s.OperatorReplies.CreateOperatorReplyChallenge(ctx, c, maxOutstandingChallenge); err != nil {
		if errors.Is(err, store.ErrTooManyChallenges) {
			writeOperatorReplyError(w, orErr(http.StatusTooManyRequests, CodeTooManyChallenges, "at most %d outstanding challenges per device", maxOutstandingChallenge))
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"challenge_id": c.ID, "nonce": encoded, "message_id": id, "option": c.Option,
		"binding": c.Binding, "expires_at": c.ExpiresAt.Format(time.RFC3339)})
}

// handleOperatorAttestationChallenge serves GET /operator-attestation/challenges/{id}
// and .../{id}/body: Tangent's view of the card it shows Chrispian and signs.
func (s *Server) handleOperatorAttestationChallenge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	if _, oe := s.operatorReplyConfig(); oe != nil {
		writeOperatorReplyError(w, oe)
		return
	}
	if _, oe := operatorDevice(r, identity.ScopeOperatorReplyView); oe != nil {
		writeOperatorReplyError(w, oe)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/operator-attestation/challenges/")
	id, sub, _ := strings.Cut(rest, "/")
	if id == "" || (sub != "" && sub != "body") {
		writeError(w, http.StatusNotFound, CodeNotFound, "not found")
		return
	}
	ctx := r.Context()
	c, err := s.OperatorReplies.OperatorReplyChallenge(ctx, id)
	switch {
	case errors.Is(err, store.ErrChallengeUnknown):
		writeOperatorReplyError(w, orErr(http.StatusBadRequest, CodeChallengeUnknown, "no challenge %s", id))
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	case c.ConsumedAt != nil:
		writeOperatorReplyError(w, orErr(http.StatusConflict, CodeReplayed, "challenge %s is consumed", id))
		return
	case !time.Now().Before(c.ExpiresAt):
		writeOperatorReplyError(w, orErr(http.StatusBadRequest, CodeChallengeExpired, "challenge %s has expired", id))
		return
	}
	p, pe, oe := s.answerableParent(ctx, c.MessageID)
	if oe != nil {
		writeOperatorReplyError(w, oe)
		return
	}
	if sub == "body" {
		writeJSON(w, http.StatusOK, map[string]any{"challenge_id": c.ID, "message_id": c.MessageID, "body": pe.Body, "body_sha256": c.BodySHA256})
		return
	}
	label, detail := optionLabel(pe, c.Option)
	excerpt, truncated := pe.Body, false
	if utf8.RuneCountInString(excerpt) > operatorReplyExcerpt {
		excerpt, truncated = string([]rune(excerpt)[:operatorReplyExcerpt]), true
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"challenge_id": c.ID, "message_id": c.MessageID, "binding": c.Binding, "kind": pe.Kind,
		"asker":   map[string]string{"handle": s.handleFor(ctx, p.From.URN()), "address": p.From.URN(), "from_verification": p.Metadata[MetaFromVerification]},
		"subject": pe.Subject, "option": map[string]string{"id": c.Option, "label": label, "detail": detail},
		"excerpt": excerpt, "excerpt_truncated": truncated, "body_sha256": c.BodySHA256,
		"full_url":  "/operator-attestation/challenges/" + c.ID + "/body",
		"device_id": c.DeviceID, "expires_at": c.ExpiresAt.Format(time.RFC3339),
	})
}

func optionLabel(pe envelope.Envelope, option string) (string, string) {
	for _, o := range pe.Options {
		if o.ID == option {
			return o.Label, o.Detail
		}
	}
	switch option {
	case envelope.OptionApprove:
		return "Approve", ""
	case envelope.OptionDeny:
		return "Deny", ""
	}
	return option, ""
}

// handleOperatorReply serves POST /messages/{id}/operator-reply, verifying in
// the contract's order and refusing on the first failure. Nothing is stored on
// a failure except that a named challenge stays consumed.
func (s *Server) handleOperatorReply(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	cfg, oe := s.operatorReplyConfig()
	if oe != nil {
		writeOperatorReplyError(w, oe)
		return
	}
	var req struct {
		ChallengeID string  `json:"challenge_id"`
		Option      string  `json:"option"`
		Note        *string `json:"note,omitempty"`
		Assertion   string  `json:"assertion"`
	}
	audit := map[string]any{"message_id": id}
	p, oe := operatorDevice(r, identity.ScopeOperatorReply)
	if oe == nil {
		audit["device_id"] = p.ID
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			oe = orErr(http.StatusBadRequest, CodeInvalidRequest, "invalid body: %v", err)
		}
	}
	var res map[string]any
	if oe == nil {
		audit["challenge_id"] = req.ChallengeID
		res, oe = s.attestedReply(r.Context(), p, id, req.ChallengeID, req.Option, req.Note, req.Assertion, cfg, audit)
	}
	event := AuditOperatorReplyAttested
	if oe != nil {
		event = AuditOperatorReplyRefused
		audit["code"] = oe.code
	}
	s.auditOperatorReply(r, p, event, audit)
	if oe != nil {
		writeOperatorReplyError(w, oe)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) attestedReply(ctx context.Context, p identity.Principal, id, challengeID, option string, note *string, assertion string, cfg *operatorattest.Config, audit map[string]any) (map[string]any, *operatorReplyError) {
	// 2. The challenge: this device's, for this parent; then consumed.
	c, err := s.OperatorReplies.OperatorReplyChallenge(ctx, challengeID)
	if errors.Is(err, store.ErrChallengeUnknown) || (err == nil && (c.DeviceID != p.ID || c.MessageID != id)) {
		return nil, orErr(http.StatusBadRequest, CodeChallengeUnknown, "no challenge %s for this device and message", challengeID)
	}
	if err != nil {
		return nil, orErr(http.StatusInternalServerError, CodeInternalError, "%v", err)
	}
	now := time.Now().UTC()
	consumed, err := s.OperatorReplies.ConsumeOperatorReplyChallenge(ctx, c.ID, now)
	if err != nil {
		return nil, orErr(http.StatusInternalServerError, CodeInternalError, "%v", err)
	}
	if !consumed {
		return nil, orErr(http.StatusConflict, CodeReplayed, "challenge %s is already consumed", c.ID)
	}
	if !now.Before(c.ExpiresAt) {
		return nil, orErr(http.StatusBadRequest, CodeChallengeExpired, "challenge %s has expired", c.ID)
	}
	// 3. Signature, with an approved key.
	claims, kid, err := operatorattest.Verify(assertion, *cfg)
	if err != nil {
		return nil, orErr(http.StatusUnauthorized, CodeAssertionInvalid, "%v", err)
	}
	audit["elevation_id"], audit["key_id"] = claims.ElevationID, kid
	// 4. Freshness: Tether's bound, whatever the operator's elevation_ttl is.
	if err = operatorattest.CheckFresh(claims, now, cfg.MaxAge); err != nil {
		return nil, orErr(http.StatusUnauthorized, CodeAssertionStale, "%v", err)
	}
	// 5. Binding to the challenge, the parent and the request.
	parent, pe, oe := s.answerableParent(ctx, id)
	if oe != nil {
		return nil, oe
	}
	audit["parent_from_verification"] = parent.Metadata[MetaFromVerification]
	if option != c.Option {
		return nil, orErr(http.StatusUnauthorized, CodeAssertionUnbound, "the request's option %q is not the challenge's", option)
	}
	if err = operatorattest.CheckBinding(claims, operatorattest.Expect{Issuer: cfg.Issuer, ChallengeID: c.ID, NonceSHA256: c.NonceSHA256,
		MessageID: id, Option: c.Option, Binding: c.Binding, BodySHA256: c.BodySHA256, DeviceID: c.DeviceID, Note: note}); err != nil {
		return nil, orErr(http.StatusUnauthorized, CodeAssertionUnbound, "%v", err)
	}
	if operatorattest.SHA256Hex(pe.Body) != c.BodySHA256 {
		return nil, orErr(http.StatusUnauthorized, CodeAssertionUnbound, "the parent's body changed since the challenge")
	}
	if note != nil && utf8.RuneCountInString(*note) > envelope.MaxNote {
		e := orErr(http.StatusBadRequest, CodeInvalidRequest, "note longer than %d characters", envelope.MaxNote)
		e.details = map[string]string{"field": "answer.note", "reason": fmt.Sprintf("at most %d characters", envelope.MaxNote)}
		return nil, e
	}
	// 6. One-use proofs.
	used, err := s.OperatorReplies.OperatorReplyProofUsed(ctx, claims.Jti, claims.ElevationID)
	if err != nil {
		return nil, orErr(http.StatusInternalServerError, CodeInternalError, "%v", err)
	}
	if used {
		return nil, orErr(http.StatusConflict, CodeReplayed, "the assertion's jti or elevation_id was already used")
	}
	// 7. One transaction: not yet answered, record the proofs, reserve the answer.
	forms := s.operatorForms(ctx)
	err = s.OperatorReplies.ReserveOperatorReply(ctx, store.OperatorReplyReservation{MessageID: id, ChallengeID: c.ID, JTI: claims.Jti, ElevationID: claims.ElevationID, At: now}, forms)
	switch {
	case errors.Is(err, store.ErrAlreadyAnswered):
		return nil, orErr(http.StatusConflict, CodeAlreadyAnswered, "the operator already answered %s", id)
	case errors.Is(err, store.ErrReplayed):
		return nil, orErr(http.StatusConflict, CodeReplayed, "the assertion's jti or elevation_id was already used")
	case err != nil:
		return nil, orErr(http.StatusInternalServerError, CodeInternalError, "%v", err)
	}
	sent, err := s.storeAttestedReply(ctx, parent, pe, c, claims, kid, note)
	if err != nil {
		if rerr := s.OperatorReplies.ReleaseOperatorReply(context.WithoutCancel(ctx), id); rerr != nil {
			err = errors.Join(err, rerr)
		}
		return nil, orErr(http.StatusInternalServerError, CodeInternalError, "store the reply: %v", err)
	}
	if err = s.OperatorReplies.CompleteOperatorReply(context.WithoutCancel(ctx), id, sent.ID); err != nil {
		return nil, orErr(http.StatusInternalServerError, CodeInternalError, "%v", err)
	}
	audit["reply_id"] = sent.ID
	return map[string]any{"message_id": sent.ID, "in_reply_to": id, "thread_id": sent.ThreadID, "option": c.Option}, nil
}

// storeAttestedReply sends the answer to the parent's sender, stamped as the
// operator's verified reply with its proof.
func (s *Server) storeAttestedReply(ctx context.Context, parent messaging.Envelope, pe envelope.Envelope, c store.OperatorReplyChallenge, claims operatorattest.Claims, kid string, note *string) (messaging.Envelope, error) {
	to := s.handleFor(ctx, parent.From.URN())
	if to == "" {
		to = parent.From.URN()
	}
	label, _ := optionLabel(pe, c.Option)
	answer := &envelope.Answer{Option: c.Option}
	if note != nil {
		answer.Note = *note
	}
	reply := envelope.Envelope{Schema: envelope.Schema, Kind: envelope.KindMessage, From: envelope.Operator, To: []string{to}, InReplyTo: c.MessageID,
		Subject: strings.TrimSpace("Re: " + pe.Subject), Body: label, Answer: answer}
	body, err := json.Marshal(reply)
	if err != nil {
		return messaging.Envelope{}, err
	}
	from, err := messaging.ParseURN(s.canonicalAlias(ctx, identity.OperatorID))
	if err != nil {
		return messaging.Envelope{}, err
	}
	proof, _ := json.Marshal(map[string]any{"device_id": claims.DeviceID, "elevation_id": claims.ElevationID, "elevated_at": claims.ElevatedAt,
		"assertion_id": claims.Jti, "challenge_id": c.ID, "key_id": kid})
	thread := parent.ThreadID
	if thread == "" {
		thread = parent.ID
	}
	return s.sendMessage(store.WithAttestedReply(ctx), messaging.Envelope{From: from, To: parent.From, Kind: envelope.TransportKind(envelope.KindMessage), ThreadID: thread,
		InReplyTo: c.MessageID, Payload: body, ContentType: envelope.ContentType, Metadata: map[string]string{
			MetaFromPrincipal: identity.OperatorID, MetaFromVerification: "verified", MetaOperatorAttestation: string(proof),
			"tether.message_id": "operator-reply:" + c.ID,
		}})
}

func (s *Server) auditOperatorReply(r *http.Request, p identity.Principal, event string, detail map[string]any) {
	if s.IdentityAudit == nil {
		return
	}
	raw, _ := json.Marshal(detail)
	_ = s.IdentityAudit(context.WithoutCancel(r.Context()), identity.Observation{At: time.Now().UTC(), PrincipalID: p.ID, Mode: s.IdentityMode,
		Authentication: "verified", Method: r.Method, Route: r.URL.Path, Event: event, Detail: string(raw)})
}
