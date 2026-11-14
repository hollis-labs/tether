// Package triage classifies mail to a hub-hosted seat before it may wake the
// seat (CW-20261114-0017, design v2 section 5: mail is a record, not a turn).
// It is a pure function of the message: the hub decides what to do with each
// class (wake now, defer under the interrupt cap, ride along on the next human
// prompt, or settle quietly).
package triage

import (
	"encoding/json"

	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/messaging/envelope"
)

// Class is what a message is worth to its recipient right now.
type Class string

const (
	// Human: wake now, never capped (the operator wrote, or a person must look).
	Human Class = "human"
	// Action: needs the seat to act; wakes, at most once per interval per seat.
	Action Class = "action"
	// FYI: no wake; one line on the seat's next human-originated turn.
	FYI Class = "fyi"
	// Read: no wake; settled quietly when the sender said nobody needs to look.
	Read Class = "read"
)

// OperatorURN is the human operator's address.
const OperatorURN = "msg://user/local/operator"

// Input is what classification looks at.
type Input struct {
	From        string // sender URN
	Kind        messaging.Kind
	ContentType string
	Payload     []byte
	Urgency     string // very-low | low | normal | high | ""
	// RecipientHandle is the seat's @handle, for envelope awaiting.
	RecipientHandle string
}

// Result is a classification and why.
type Result struct {
	Class Class
	// Reason is a short machine-readable rule name.
	Reason string
	// Typed is true when the message is a typed envelope.
	Typed bool
	// AutoSettle is true when the hub may mark it read itself: only a typed
	// envelope whose sender set attention=none.
	AutoSettle bool
}

// Classify applies the shared order human > action > read > fyi (the same as
// the local tether-mail triage): the first class with a matching rule wins, so
// attention=none never overrides an action rule.
//
//	human   the operator wrote                                  (from_operator)
//	        envelope attention=human (as set by the sender, or   (attention_human)
//	        the default when the operator is to/awaiting)
//	action  urgency high                                        (urgency_high)
//	        envelope awaiting this seat                         (awaiting_recipient)
//	        envelope question|decision|approval_request|
//	                 escalation|handoff                         (envelope_kind)
//	        untyped request|escalation|handoff                  (transport_kind)
//	read    envelope attention=none (explicit), auto-settled    (attention_none)
//	        untyped urgency very-low, left unread               (urgency_very_low)
//	fyi     envelope update|message                             (envelope_fyi)
//	        untyped notice|status_update|response               (transport_fyi)
//	action  anything else                                       (default)
func Classify(in Input) Result {
	var env envelope.Envelope
	typed := in.ContentType == envelope.ContentType && json.Unmarshal(in.Payload, &env) == nil && env.Kind != ""
	// human
	if in.From == OperatorURN {
		return Result{Class: Human, Reason: "from_operator", Typed: typed}
	}
	if typed && env.EffectiveAttention() == envelope.AttentionHuman {
		return Result{Class: Human, Reason: "attention_human", Typed: true}
	}
	// action
	if in.Urgency == "high" {
		return Result{Class: Action, Reason: "urgency_high", Typed: typed}
	}
	if typed {
		if env.Awaiting != "" && env.Awaiting == in.RecipientHandle {
			return Result{Class: Action, Reason: "awaiting_recipient", Typed: true}
		}
		switch env.Kind {
		case envelope.KindQuestion, envelope.KindDecision, envelope.KindApprovalRequest, envelope.KindEscalation, envelope.KindHandoff:
			return Result{Class: Action, Reason: "envelope_kind", Typed: true}
		case envelope.KindUpdate, envelope.KindMessage:
			// read or fyi below
		}
	} else {
		switch in.Kind {
		case messaging.MsgKindRequest, messaging.MsgKindEscalation, messaging.MsgKindHandoff:
			return Result{Class: Action, Reason: "transport_kind"}
		case messaging.MsgKindNotice, messaging.MsgKindStatusUpdate, messaging.MsgKindResponse:
			// read or fyi below
		}
	}
	// read
	if typed && env.Attention == envelope.AttentionNone {
		return Result{Class: Read, Reason: "attention_none", Typed: true, AutoSettle: true}
	}
	if !typed && in.Urgency == "very-low" {
		return Result{Class: Read, Reason: "urgency_very_low"}
	}
	// fyi
	if typed && (env.Kind == envelope.KindUpdate || env.Kind == envelope.KindMessage) {
		return Result{Class: FYI, Reason: "envelope_fyi", Typed: true}
	}
	if !typed {
		switch in.Kind {
		case messaging.MsgKindNotice, messaging.MsgKindStatusUpdate, messaging.MsgKindResponse:
			return Result{Class: FYI, Reason: "transport_fyi"}
		case messaging.MsgKindRequest, messaging.MsgKindEscalation, messaging.MsgKindHandoff:
		}
	}
	return Result{Class: Action, Reason: "default", Typed: typed}
}

// Wakes reports whether the class may wake the seat at all.
func (c Class) Wakes() bool { return c == Human || c == Action }
