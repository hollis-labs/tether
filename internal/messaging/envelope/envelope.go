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
)

// Kinds lists every kind, in schema order.
var Kinds = []Kind{KindQuestion, KindUpdate, KindHandoff, KindEscalation, KindApprovalRequest, KindDecision, KindMessage}

// Attention says who must look at it.
type Attention string

const (
	AttentionHuman Attention = "human"
	AttentionAgent Attention = "agent"
	AttentionNone  Attention = "none"
)

// Operator is the reserved handle of the human operator.
const Operator = "@chrispian"

// Implicit option ids of an approval_request sent without options.
const (
	OptionApprove = "approve"
	OptionDeny    = "deny"
)

// Limits.
const (
	MaxRecipients = 10
	MaxSubject    = 120
	MaxBody       = 4000
	MinOptions    = 2
	MaxOptions    = 6
	MaxLabel      = 120
	MaxDetail     = 500
	MaxNote       = 1000
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

// Normalize fills the defaults: schema, and attention (human when the operator
// is a recipient or awaited, else agent). It trims handles and text edges.
func (e *Envelope) Normalize() {
	e.Schema = Schema
	for i, h := range e.To {
		e.To[i] = strings.TrimSpace(h)
	}
	e.Awaiting = strings.TrimSpace(e.Awaiting)
	e.Subject = strings.TrimSpace(e.Subject)
	e.Body = strings.TrimSpace(e.Body)
	if e.Attention == "" {
		e.Attention = AttentionAgent
		if e.Awaiting == Operator || containsHandle(e.To, Operator) {
			e.Attention = AttentionHuman
		}
	}
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
	if strings.TrimSpace(e.Body) == "" {
		return fail("body", "is required")
	}
	if n := utf8.RuneCountInString(e.Body); n > MaxBody {
		return fail("body", "is %d characters; at most %d (the receiver asks for more)", n, MaxBody)
	}
	if err := e.validOptions(); err != nil {
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

func (e Envelope) validOptions() error {
	if !HasOptions(e.Kind) {
		if len(e.Options) > 0 {
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
	case KindQuestion, KindApprovalRequest, KindDecision:
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
