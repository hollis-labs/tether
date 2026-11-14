package envelope

import (
	"errors"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/mesh/core/messaging"
)

type handles map[string]string

func (h handles) Resolve(handle string) (string, bool) { u, ok := h[handle]; return u, ok }

var dir = handles{
	"@archie":         "msg://agent/agent-mux/agt_lc7m97f5fp",
	"@photon-tachyon": "msg://agent/agent-mux/agt_4jfp86a7kp",
	"@chrispian":      "msg://user/local/operator",
}

func valid() Envelope {
	return Envelope{Kind: KindQuestion, To: []string{"@archie"}, Re: "task:CW-20261114-0013", Subject: "Ship both?", Body: "Deploy #538 with #539?",
		Options: []Option{{ID: "a", Label: "Ship both", Recommended: true}, {ID: "b", Label: "Ship #538 only"}}, DefaultOption: "a"}
}

func fieldOf(t *testing.T, err error) string {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v is not an *Error", err)
	}
	if e.Reason == "" {
		t.Fatalf("error on %s has no reason", e.Field)
	}
	return e.Field
}

func TestValidEnvelopeRoundTrip(t *testing.T) {
	e := valid()
	e.Normalize()
	urns, err := Check(e, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Attention is stored only when the sender set it; the default is derived.
	if len(urns) != 1 || urns[0] != dir["@archie"] || e.Schema != Schema || e.Attention != "" || e.EffectiveAttention() != AttentionAgent {
		t.Fatalf("urns=%v schema=%q attention=%q", urns, e.Schema, e.Attention)
	}
	// An answer to it.
	parent := e
	reply := Envelope{Kind: KindMessage, To: []string{"@archie"}, InReplyTo: "m1", Body: "Go.", Answer: &Answer{Option: "a"}}
	reply.Normalize()
	if _, err := Check(reply, dir, &Parent{Envelope: &parent}); err != nil {
		t.Fatal(err)
	}
}

func TestMalformedEnvelopesAreRejectedWithFieldAndReason(t *testing.T) {
	cases := map[string]struct {
		edit  func(*Envelope)
		field string
	}{
		"unknown kind":          {func(e *Envelope) { e.Kind = "chat" }, "kind"},
		"no recipient":          {func(e *Envelope) { e.To = nil }, "to"},
		"bare name":             {func(e *Envelope) { e.To = []string{"archie"} }, "to[0]"},
		"duplicate recipient":   {func(e *Envelope) { e.To = []string{"@archie", "@archie"} }, "to[1]"},
		"bad re":                {func(e *Envelope) { e.Re = "CW-20261114-0013" }, "re"},
		"bad attention":         {func(e *Envelope) { e.Attention = "urgent" }, "attention"},
		"bad awaiting":          {func(e *Envelope) { e.Awaiting = "archie" }, "awaiting"},
		"bad intent":            {func(e *Envelope) { e.Intent = "Deploy Now" }, "intent"},
		"two-line subject":      {func(e *Envelope) { e.Subject = "a\nb" }, "subject"},
		"empty body":            {func(e *Envelope) { e.Body = "  " }, "body"},
		"long body":             {func(e *Envelope) { e.Body = strings.Repeat("x", MaxBody+1) }, "body"},
		"question no options":   {func(e *Envelope) { e.Options, e.DefaultOption = nil, "" }, "options"},
		"one option":            {func(e *Envelope) { e.Options, e.DefaultOption = e.Options[:1], "" }, "options"},
		"dup option id":         {func(e *Envelope) { e.Options[1].ID = "a" }, "options[1].id"},
		"two recommended":       {func(e *Envelope) { e.Options[1].Recommended = true }, "options"},
		"empty label":           {func(e *Envelope) { e.Options[0].Label = "" }, "options[0].label"},
		"default not an option": {func(e *Envelope) { e.DefaultOption = "z" }, "default_option"},
		"options on an update":  {func(e *Envelope) { e.Kind = KindUpdate }, "options"},
		"answer without reply":  {func(e *Envelope) { e.Answer = &Answer{Option: "a"} }, "answer"},
		"wrong schema":          {func(e *Envelope) { e.Schema = "v0" }, "schema"},
	}
	for name, tc := range cases {
		e := valid()
		tc.edit(&e)
		_, err := Check(e, dir, nil)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if got := fieldOf(t, err); got != tc.field {
			t.Errorf("%s: field %q, want %q (%v)", name, got, tc.field, err)
		}
	}
}

func TestUnknownHandlesAreRejected(t *testing.T) {
	e := valid()
	e.To = []string{"@archie", "@unknown"}
	_, err := Check(e, dir, nil)
	if err == nil || fieldOf(t, err) != "to[1]" || !strings.Contains(err.Error(), "unknown handle @unknown") {
		t.Fatalf("err = %v", err)
	}
	e = valid()
	e.Awaiting = "@ghost"
	if _, err := Check(e, dir, nil); err == nil || fieldOf(t, err) != "awaiting" {
		t.Fatalf("unknown awaiting: %v", err)
	}
}

func TestApprovalRequestWithoutOptionsAnswersApproveOrDeny(t *testing.T) {
	parent := Envelope{Kind: KindApprovalRequest, To: []string{"@chrispian"}, Body: "Restart hub-tether?"}
	parent.Normalize()
	if _, err := Check(parent, dir, nil); err != nil {
		t.Fatal(err)
	}
	if parent.Attention != "" || parent.EffectiveAttention() != AttentionHuman {
		t.Fatalf("attention stored %q, effective %q; want none stored and human derived for the operator", parent.Attention, parent.EffectiveAttention())
	}
	explicit := Envelope{Kind: KindUpdate, To: []string{"@chrispian"}, Attention: AttentionNone, Body: "x"}
	explicit.Normalize()
	if explicit.Attention != AttentionNone || explicit.EffectiveAttention() != AttentionNone {
		t.Fatal("an explicit attention was overwritten")
	}
	for option, ok := range map[string]bool{"approve": true, "deny": true, "a": false} {
		reply := Envelope{Kind: KindMessage, To: []string{"@archie"}, InReplyTo: "m1", Body: "ok", Answer: &Answer{Option: option}}
		_, err := Check(reply, dir, &Parent{Envelope: &parent})
		if (err == nil) != ok {
			t.Errorf("answer %q: err=%v", option, err)
		}
	}
	// An answer to something that is not a question is refused.
	update := Envelope{Kind: KindUpdate, To: []string{"@archie"}, Body: "fyi"}
	reply := Envelope{Kind: KindMessage, To: []string{"@archie"}, InReplyTo: "m2", Body: "ok", Answer: &Answer{Option: "approve"}}
	if _, err := Check(reply, dir, &Parent{Envelope: &update}); err == nil || fieldOf(t, err) != "answer" {
		t.Fatalf("answer to an update: %v", err)
	}
}

func TestDecodeRefusesUnknownFields(t *testing.T) {
	if _, err := Decode([]byte(`{"kind":"message","to":["@archie"],"body":"hi","bodyy":"typo"}`)); err == nil {
		t.Fatal("unknown field accepted")
	}
	e, err := Decode([]byte(`{"kind":"message","to":["@archie"],"body":"hi"}`))
	if err != nil || e.Kind != KindMessage {
		t.Fatalf("decode: %v %+v", err, e)
	}
}

func TestTransportKind(t *testing.T) {
	want := map[Kind]messaging.Kind{
		KindQuestion: messaging.MsgKindRequest, KindApprovalRequest: messaging.MsgKindRequest, KindDecision: messaging.MsgKindRequest,
		KindUpdate: messaging.MsgKindStatusUpdate, KindHandoff: messaging.MsgKindHandoff, KindEscalation: messaging.MsgKindEscalation, KindMessage: messaging.MsgKindNotice,
	}
	for k, w := range want {
		if got := TransportKind(k); got != w {
			t.Errorf("%s -> %s, want %s", k, got, w)
		}
	}
}
