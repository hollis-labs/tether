package triage

import (
	"encoding/json"
	"testing"

	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/messaging/envelope"
)

func typed(e envelope.Envelope) Input {
	raw, _ := json.Marshal(e)
	return Input{From: "msg://agent/agent-mux/agt_x", Kind: envelope.TransportKind(e.Kind), ContentType: envelope.ContentType, Payload: raw, RecipientHandle: "@me"}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		in     Input
		class  Class
		reason string
		settle bool
	}{
		{"operator", Input{From: OperatorURN, Kind: messaging.MsgKindNotice}, Human, "from_operator", false},
		{"attention human", typed(envelope.Envelope{Kind: envelope.KindUpdate, Attention: envelope.AttentionHuman}), Human, "attention_human", false},
		{"attention none on a question stays action", typed(envelope.Envelope{Kind: envelope.KindQuestion, Attention: envelope.AttentionNone}), Action, "envelope_kind", false},
		{"attention none on an update is read", typed(envelope.Envelope{Kind: envelope.KindUpdate, Attention: envelope.AttentionNone}), Read, "attention_none", true},
		{"attention none awaiting me stays action", typed(envelope.Envelope{Kind: envelope.KindUpdate, Attention: envelope.AttentionNone, Awaiting: "@me"}), Action, "awaiting_recipient", false},
		{"default attention to the operator is human", typed(envelope.Envelope{Kind: envelope.KindUpdate, To: []string{"@chrispian"}}), Human, "attention_human", false},
		{"no attention set is fyi for an update", typed(envelope.Envelope{Kind: envelope.KindUpdate}), FYI, "envelope_fyi", false},
		{"question", typed(envelope.Envelope{Kind: envelope.KindQuestion, Attention: envelope.AttentionAgent}), Action, "envelope_kind", false},
		{"handoff", typed(envelope.Envelope{Kind: envelope.KindHandoff, Attention: envelope.AttentionAgent}), Action, "envelope_kind", false},
		{"update", typed(envelope.Envelope{Kind: envelope.KindUpdate, Attention: envelope.AttentionAgent}), FYI, "envelope_fyi", false},
		{"update awaiting me", typed(envelope.Envelope{Kind: envelope.KindUpdate, Attention: envelope.AttentionAgent, Awaiting: "@me"}), Action, "awaiting_recipient", false},
		{"untyped request", Input{Kind: messaging.MsgKindRequest}, Action, "transport_kind", false},
		{"untyped request very-low stays action", Input{Kind: messaging.MsgKindRequest, Urgency: "very-low"}, Action, "transport_kind", false},
		{"untyped notice", Input{Kind: messaging.MsgKindNotice}, FYI, "transport_fyi", false},
		{"untyped notice high", Input{Kind: messaging.MsgKindNotice, Urgency: "high"}, Action, "urgency_high", false},
		{"untyped notice very-low", Input{Kind: messaging.MsgKindNotice, Urgency: "very-low"}, Read, "urgency_very_low", false},
		{"untyped notice high beats very-low order", Input{Kind: messaging.MsgKindNotice, Urgency: "high"}, Action, "urgency_high", false},
		{"bad envelope payload is untyped", Input{Kind: messaging.MsgKindNotice, ContentType: envelope.ContentType, Payload: []byte("{")}, FYI, "transport_fyi", false},
	}
	for _, tc := range cases {
		r := Classify(tc.in)
		if r.Class != tc.class || r.Reason != tc.reason || r.AutoSettle != tc.settle {
			t.Errorf("%s: %+v, want %s/%s settle=%v", tc.name, r, tc.class, tc.reason, tc.settle)
		}
	}
	if !Human.Wakes() || !Action.Wakes() || FYI.Wakes() || Read.Wakes() {
		t.Fatal("Wakes")
	}
}
