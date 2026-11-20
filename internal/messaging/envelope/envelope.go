// Package envelope is the typed message envelope (CW-20261114-0013, design v2
// section 5): everything a seat addresses to the system, another seat or
// Chrispian. It is stored as a message payload with ContentType, so existing
// mailbox readers still see an ordinary message. Validation is the server's:
// a malformed envelope is refused with the field and the reason, and the model
// fixes it and retries.
package envelope

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/hollis-labs/libs/mesh/core/messaging"
)

// Schema and ContentType identify a stored envelope.
const (
	Schema      = "tether.envelope/v1"
	ContentType = "application/vnd.tether.envelope.v1+json"
)

// Kind is what the envelope is for; Tachyon renders each as a card.
type Kind string

const (
	KindQuestion        Kind = "question"
	KindUpdate          Kind = "update"
	KindHandoff         Kind = "handoff"
	KindEscalation      Kind = "escalation"
	KindApprovalRequest Kind = "approval_request"
	KindDecision        Kind = "decision"
	KindMessage         Kind = "message"
	// KindRequest is "please do X and report back" (CW-20261117-0003): it
	// carries a deliverable (what done looks like) and no options.
	KindRequest Kind = "request"
)

// Kinds lists every kind, in schema order.
var Kinds = []Kind{KindQuestion, KindUpdate, KindHandoff, KindEscalation, KindApprovalRequest, KindDecision, KindMessage, KindRequest}

// Attention says who must look at it.
type Attention string

const (
	AttentionHuman Attention = "human"
	AttentionAgent Attention = "agent"
	AttentionNone  Attention = "none"
)

// Operator is the reserved handle of the human operator.
const Operator = "@chrispian"

// DraftedBy says who wrote mail sent on someone's behalf (CW-20261114-0116).
type DraftedBy string

const (
	DraftedByAssistant DraftedBy = "assistant"
	DraftedByHuman     DraftedBy = "human"
)

// ReplyStatus is how a reply settles the ask it answers (CW-20261117-0003).
type ReplyStatus string

const (
	StatusDone     ReplyStatus = "done"
	StatusDeclined ReplyStatus = "declined"
	StatusBlocked  ReplyStatus = "blocked"
)

// Implicit option ids of an approval_request sent without options.
const (
	OptionApprove = "approve"
	OptionDeny    = "deny"
)

// Limits.
const (
	MaxRecipients  = 10
	MaxSubject     = 120
	MaxBody        = 4000
	MinOptions     = 2
	MaxOptions     = 6
	MaxLabel       = 120
	MaxDetail      = 500
	MaxNote        = 1000
	MaxDeliverable = 1000
	MaxEvidence    = 10
	MaxEvidenceRef = 500
)

// Option is one choice on a question, decision or approval_request.
type Option struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Recommended bool   `json:"recommended,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// Answer is a reply's choice among its parent's options.
type Answer struct {
	Option string `json:"option"`
	Note   string `json:"note,omitempty"`
}

// Envelope is the typed message. From is stamped by the server.
type Envelope struct {
	// TaskID links a request to its durable Torque lifecycle.
	TaskID        string    `json:"task_id,omitempty"`
	Schema        string    `json:"schema,omitempty"`
	Kind          Kind      `json:"kind"`
	From          string    `json:"from,omitempty"`
	To            []string  `json:"to"`
	Re            string    `json:"re,omitempty"`
	InReplyTo     string    `json:"in_reply_to,omitempty"`
	Attention     Attention `json:"attention,omitempty"`
	Awaiting      string    `json:"awaiting,omitempty"`
	Intent        string    `json:"intent,omitempty"`
	Subject       string    `json:"subject,omitempty"`
	Body          string    `json:"body"`
	Options       []Option  `json:"options,omitempty"`
	DefaultOption string    `json:"default_option,omitempty"`
	Answer        *Answer   `json:"answer,omitempty"`
	// Deliverable is what "done" looks like; required on a request.
	Deliverable string `json:"deliverable,omitempty"`
	// Status settles the ask a reply answers: done, declined or blocked.
	Status ReplyStatus `json:"status,omitempty"`
	// Evidence lists pointers (PR, commit, path, URL) a reply offers.
	Evidence []string `json:"evidence,omitempty"`
	// OnBehalfOf names who the sender acts for (an @handle or a principal URN),
	// and DraftedBy who wrote it (CW-20261114-0116): an assistant sending for
	// Chrispian from its own address. It is the sender's claim, not an identity:
	// it never changes who the mail is from, and mail from an agent that says it
	// acts for someone is never triaged as that person (triage.Classify).
	OnBehalfOf string    `json:"on_behalf_of,omitempty"`
	DraftedBy  DraftedBy `json:"drafted_by,omitempty"`
}

// Error is a validation failure: which field, and why.
type Error struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

func (e *Error) Error() string { return "envelope." + e.Field + ": " + e.Reason }

func fail(field, format string, args ...any) *Error {
	return &Error{Field: field, Reason: fmt.Sprintf(format, args...)}
}

var (
	handleRE = regexp.MustCompile(`^@[a-z][a-z0-9_-]{0,63}$`)
	intentRE = regexp.MustCompile(`^[a-z0-9._-]{1,40}$`)
	optionRE = regexp.MustCompile(`^[a-z0-9_-]{1,20}$`)
	taskRE   = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[0-9]{8}-[0-9]{4}$`)
	slugRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,79}$`)
)

// ValidHandle reports whether h is a syntactically valid @handle.
func ValidHandle(h string) bool { return handleRE.MatchString(h) }

// Decode reads an envelope from JSON, refusing unknown fields so a typo is an
// error rather than a silently dropped field.
func Decode(raw []byte) (Envelope, error) {
	var e Envelope
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return Envelope{}, &Error{Field: "(json)", Reason: err.Error()}
	}
	return e, nil
}

// Normalize sets the schema and trims handles and text edges. It does not fill
// attention: a stored envelope carries attention only when the sender set it,
// so triage can tell an explicit choice from the default (EffectiveAttention).
func (e *Envelope) Normalize() {
	e.Schema = Schema
	for i, h := range e.To {
		e.To[i] = strings.TrimSpace(h)
	}
	e.Awaiting = strings.TrimSpace(e.Awaiting)
	e.Subject = strings.TrimSpace(e.Subject)
	e.Body = strings.TrimSpace(e.Body)
	e.Deliverable = strings.TrimSpace(e.Deliverable)
	if e.Subject == "" && e.Kind != KindRequest && NeedsSubject(e.Kind) {
		// Question, decision and approval_request predate the subject rule: their
		// subject defaults to the body's first line, as tether_message_post
		// documents. A request (new) must name one (CW-20261117-0003).
		e.Subject = firstLine(e.Body, MaxSubject)
	}
	for i, ev := range e.Evidence {
		e.Evidence[i] = strings.TrimSpace(ev)
	}
	e.OnBehalfOf = strings.TrimSpace(e.OnBehalfOf)
	if e.OnBehalfOf != "" && e.DraftedBy == "" {
		e.DraftedBy = DraftedByAssistant
	}
}

// Behalf reads the on-behalf marker of a stored message: its on_behalf_of and
// drafted_by when the payload is a typed envelope that carries one, else "".
func Behalf(contentType string, payload []byte) (onBehalfOf string, draftedBy DraftedBy) {
	if contentType != ContentType {
		return "", ""
	}
	var e struct {
		OnBehalfOf string    `json:"on_behalf_of"`
		DraftedBy  DraftedBy `json:"drafted_by"`
	}
	if json.Unmarshal(payload, &e) != nil || e.OnBehalfOf == "" {
		return "", ""
	}
	if e.DraftedBy == "" {
		e.DraftedBy = DraftedByAssistant
	}
	return e.OnBehalfOf, e.DraftedBy
}

// EffectiveAttention is the attention the sender set, else the default: human
// when the operator is a recipient or awaited, otherwise agent.
func (e Envelope) EffectiveAttention() Attention {
	if e.Attention != "" {
		return e.Attention
	}
	if e.Awaiting == Operator || containsHandle(e.To, Operator) {
		return AttentionHuman
	}
	return AttentionAgent
}

func containsHandle(hs []string, h string) bool {
	for _, x := range hs {
		if x == h {
			return true
		}
	}
	return false
}

// HasOptions reports whether kind carries options (or implicit approve/deny).
func HasOptions(k Kind) bool {
	return k == KindQuestion || k == KindDecision || k == KindApprovalRequest
}

// Validate checks shape and syntax. It does not resolve handles or look up the
// parent of a reply; Check does both.
func (e Envelope) Validate() error {
	if e.TaskID != "" && !taskRE.MatchString(e.TaskID) {
		return fail("task_id", "must be a Torque task id")
	}
	if e.Schema != "" && e.Schema != Schema {
		return fail("schema", "must be %q", Schema)
	}
	known := false
	for _, k := range Kinds {
		known = known || e.Kind == k
	}
	if !known {
		return fail("kind", "must be one of %s", kindList())
	}
	if len(e.To) == 0 {
		return fail("to", "at least one recipient handle is required")
	}
	if len(e.To) > MaxRecipients {
		return fail("to", "at most %d recipients", MaxRecipients)
	}
	seen := map[string]bool{}
	for i, h := range e.To {
		if !ValidHandle(h) {
			return fail(fmt.Sprintf("to[%d]", i), "%q is not a handle (want @name: lowercase letters, digits, '-' or '_')", h)
		}
		if seen[h] {
			return fail(fmt.Sprintf("to[%d]", i), "%s is listed twice", h)
		}
		seen[h] = true
	}
	if err := validRe(e.Re); err != nil {
		return err
	}
	switch e.Attention {
	case "", AttentionHuman, AttentionAgent, AttentionNone:
	default:
		return fail("attention", "must be human, agent or none")
	}
	if e.Awaiting != "" && !ValidHandle(e.Awaiting) {
		return fail("awaiting", "%q is not a handle", e.Awaiting)
	}
	if e.Intent != "" && !intentRE.MatchString(e.Intent) {
		return fail("intent", "must match [a-z0-9._-]{1,40}")
	}
	if n := utf8.RuneCountInString(e.Subject); n > MaxSubject || strings.ContainsAny(e.Subject, "\r\n") {
		return fail("subject", "one line of at most %d characters", MaxSubject)
	}
	if e.Subject == "" && NeedsSubject(e.Kind) {
		return fail("subject", "is required for %s (one line, at most %d characters)", e.Kind, MaxSubject)
	}
	if strings.TrimSpace(e.Body) == "" {
		return fail("body", "is required")
	}
	if err := e.validRequest(); err != nil {
		return err
	}
	if n := utf8.RuneCountInString(e.Body); n > MaxBody {
		return fail("body", "is %d characters; at most %d (the receiver asks for more)", n, MaxBody)
	}
	if err := e.validOptions(); err != nil {
		return err
	}
	if err := e.validBehalf(); err != nil {
		return err
	}
	if e.Answer != nil {
		if e.InReplyTo == "" {
			return fail("answer", "only a reply (in_reply_to) carries an answer")
		}
		if !optionRE.MatchString(e.Answer.Option) {
			return fail("answer.option", "is required and must match [a-z0-9_-]{1,20}")
		}
		if utf8.RuneCountInString(e.Answer.Note) > MaxNote {
			return fail("answer.note", "at most %d characters", MaxNote)
		}
	}
	return nil
}

func firstLine(text string, limit int) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > limit {
		line = string(r[:limit-1]) + "…"
	}
	return line
}

// NeedsSubject reports whether kind must carry a subject: anything that asks
// for an answer or an action (CW-20261117-0003).
func NeedsSubject(k Kind) bool {
	return k == KindRequest || k == KindQuestion || k == KindDecision || k == KindApprovalRequest
}

func (e Envelope) validRequest() error {
	if e.Kind == KindRequest && strings.TrimSpace(e.Deliverable) == "" {
		return fail("deliverable", "is required for a request: say what done looks like")
	}
	if e.Deliverable != "" {
		if e.Kind != KindRequest {
			return fail("deliverable", "only a request carries a deliverable")
		}
		if utf8.RuneCountInString(e.Deliverable) > MaxDeliverable {
			return fail("deliverable", "at most %d characters", MaxDeliverable)
		}
	}
	switch e.Status {
	case "":
	case StatusDone, StatusDeclined, StatusBlocked:
		if e.InReplyTo == "" {
			return fail("status", "only a reply (in_reply_to) carries a status")
		}
	default:
		return fail("status", "must be done, declined or blocked")
	}
	if len(e.Evidence) > MaxEvidence {
		return fail("evidence", "at most %d items", MaxEvidence)
	}
	for i, ev := range e.Evidence {
		if ev == "" || utf8.RuneCountInString(ev) > MaxEvidenceRef || strings.ContainsAny(ev, "\r\n") {
			return fail(fmt.Sprintf("evidence[%d]", i), "one line of 1 to %d characters", MaxEvidenceRef)
		}
	}
	return nil
}

func (e Envelope) validBehalf() error {
	switch e.DraftedBy {
	case "", DraftedByAssistant, DraftedByHuman:
	default:
		return fail("drafted_by", "must be assistant or human")
	}
	if e.OnBehalfOf == "" {
		if e.DraftedBy != "" {
			return fail("drafted_by", "only mail sent on_behalf_of someone says who drafted it")
		}
		return nil
	}
	if ValidHandle(e.OnBehalfOf) {
		return nil
	}
	if _, err := messaging.ParseURN(e.OnBehalfOf); err != nil {
		return fail("on_behalf_of", "%q is neither a @handle nor a principal URN", e.OnBehalfOf)
	}
	return nil
}

func (e Envelope) validOptions() error {
	if !HasOptions(e.Kind) {
		if len(e.Options) > 0 {
			if e.Kind == KindRequest {
				return fail("options", "a request takes no options (use a question or decision to offer choices)")
			}
			return fail("options", "only question, decision and approval_request carry options")
		}
		if e.DefaultOption != "" {
			return fail("default_option", "only question, decision and approval_request carry options")
		}
		return nil
	}
	if len(e.Options) == 0 && e.Kind != KindApprovalRequest {
		return fail("options", "%s needs %d to %d options", e.Kind, MinOptions, MaxOptions)
	}
	if len(e.Options) > 0 && (len(e.Options) < MinOptions || len(e.Options) > MaxOptions) {
		return fail("options", "%d to %d options", MinOptions, MaxOptions)
	}
	ids := map[string]bool{}
	recommended := 0
	for i, o := range e.Options {
		field := fmt.Sprintf("options[%d]", i)
		if !optionRE.MatchString(o.ID) {
			return fail(field+".id", "must match [a-z0-9_-]{1,20}")
		}
		if ids[o.ID] {
			return fail(field+".id", "%q is used twice", o.ID)
		}
		ids[o.ID] = true
		if l := strings.TrimSpace(o.Label); l == "" || utf8.RuneCountInString(l) > MaxLabel || strings.ContainsAny(l, "\r\n") {
			return fail(field+".label", "one line of 1 to %d characters", MaxLabel)
		}
		if utf8.RuneCountInString(o.Detail) > MaxDetail {
			return fail(field+".detail", "at most %d characters", MaxDetail)
		}
		if o.Recommended {
			recommended++
		}
	}
	if recommended > 1 {
		return fail("options", "at most one option is recommended")
	}
	if e.DefaultOption != "" && !e.OptionIDs()[e.DefaultOption] {
		return fail("default_option", "%q is not one of the option ids", e.DefaultOption)
	}
	return nil
}

// OptionIDs are the ids a reply may answer with: the options, or approve/deny
// for an approval_request sent without options.
func (e Envelope) OptionIDs() map[string]bool {
	ids := map[string]bool{}
	for _, o := range e.Options {
		ids[o.ID] = true
	}
	if len(ids) == 0 && e.Kind == KindApprovalRequest {
		ids[OptionApprove], ids[OptionDeny] = true, true
	}
	return ids
}

func validRe(re string) error {
	if re == "" {
		return nil
	}
	scheme, ref, ok := strings.Cut(re, ":")
	switch {
	case !ok:
	case scheme == "task" && taskRE.MatchString(ref):
		return nil
	case scheme == "thread" && ref != "" && len(ref) <= 128 && !strings.ContainsAny(ref, " \r\n"):
		return nil
	case scheme == "topic" && slugRE.MatchString(ref):
		return nil
	}
	return fail("re", "must be task:<task id>, thread:<id> or topic:<slug>")
}

func kindList() string {
	s := make([]string, len(Kinds))
	for i, k := range Kinds {
		s[i] = string(k)
	}
	return strings.Join(s, ", ")
}

// TransportKind maps an envelope kind onto the mesh message kind, so a reader
// that does not know envelopes still sees a sensible message.
func TransportKind(k Kind) messaging.Kind {
	switch k {
	case KindQuestion, KindApprovalRequest, KindDecision, KindRequest:
		return messaging.MsgKindRequest
	case KindUpdate:
		return messaging.MsgKindStatusUpdate
	case KindHandoff:
		return messaging.MsgKindHandoff
	case KindEscalation:
		return messaging.MsgKindEscalation
	case KindMessage:
		return messaging.MsgKindNotice
	}
	return messaging.MsgKindNotice
}

// Resolver maps a handle to the address it names. ok is false for an unknown
// handle.
type Resolver interface {
	Resolve(handle string) (urn string, ok bool)
}

// Parent is the message a reply answers: its envelope when it was typed.
type Parent struct {
	Envelope *Envelope
}

// Check validates e fully for sending: shape, every handle resolves, and an
// answer fits its parent's options. It returns the resolved recipient URNs in
// the order of e.To. parent is nil when e is not a reply or the parent is not
// readable by the sender (the caller decides which is an error).
func Check(e Envelope, r Resolver, parent *Parent) ([]string, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	urns := make([]string, len(e.To))
	for i, h := range e.To {
		urn, ok := r.Resolve(h)
		if !ok {
			return nil, fail(fmt.Sprintf("to[%d]", i), "unknown handle %s", h)
		}
		urns[i] = urn
	}
	if e.Awaiting != "" {
		if _, ok := r.Resolve(e.Awaiting); !ok {
			return nil, fail("awaiting", "unknown handle %s", e.Awaiting)
		}
	}
	if ValidHandle(e.OnBehalfOf) {
		if _, ok := r.Resolve(e.OnBehalfOf); !ok {
			return nil, fail("on_behalf_of", "unknown handle %s", e.OnBehalfOf)
		}
	}
	if e.Answer != nil {
		if parent == nil || parent.Envelope == nil || !HasOptions(parent.Envelope.Kind) {
			return nil, fail("answer", "in_reply_to is not a question, decision or approval_request")
		}
		if !parent.Envelope.OptionIDs()[e.Answer.Option] {
			return nil, fail("answer.option", "%q is not one of the options of %s", e.Answer.Option, e.InReplyTo)
		}
	}
	return urns, nil
}
