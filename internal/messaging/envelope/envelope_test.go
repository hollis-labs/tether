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

// CW-20261114-0116: on_behalf_of names who the sender acts for (a handle that must resolve, or a URN);
// drafted_by says who wrote it, only beside on_behalf_of, and defaults to assistant.
func TestOnBehalfOf(t *testing.T) {
	e := Envelope{Kind: KindMessage, To: []string{"@archie"}, Body: "Chrispian asks for the deploy notes.", OnBehalfOf: " @chrispian "}
	e.Normalize()
	if e.OnBehalfOf != "@chrispian" || e.DraftedBy != DraftedByAssistant {
		t.Fatalf("normalized: %q %q", e.OnBehalfOf, e.DraftedBy)
	}
	if _, err := Check(e, dir, nil); err != nil {
		t.Fatal(err)
	}
	urn := e
	urn.OnBehalfOf, urn.DraftedBy = "msg://user/local/operator", DraftedByHuman
	if _, err := Check(urn, dir, nil); err != nil {
		t.Fatalf("a principal URN: %v", err)
	}
	for name, c := range map[string]struct {
		edit  func(*Envelope)
		field string
	}{
		"drafted_by alone":       {func(e *Envelope) { e.OnBehalfOf, e.DraftedBy = "", DraftedByHuman }, "drafted_by"},
		"bad drafted_by":         {func(e *Envelope) { e.DraftedBy = "robot" }, "drafted_by"},
		"neither handle nor urn": {func(e *Envelope) { e.OnBehalfOf = "chrispian" }, "on_behalf_of"},
		"unknown handle":         {func(e *Envelope) { e.OnBehalfOf = "@nobody" }, "on_behalf_of"},
	} {
		bad := e
		c.edit(&bad)
		if _, err := Check(bad, dir, nil); err == nil || fieldOf(t, err) != c.field {
			t.Fatalf("%s: %v", name, err)
		}
	}
	raw := []byte(`{"kind":"message","to":["@archie"],"body":"x","on_behalf_of":"@chrispian"}`)
	if who, by := Behalf(ContentType, raw); who != "@chrispian" || by != DraftedByAssistant {
		t.Fatalf("Behalf = %q %q", who, by)
	}
	if who, _ := Behalf("application/json", raw); who != "" {
		t.Fatal("an untyped payload has no marker")
	}
}

// CW-20261117-0003: the request kind, its deliverable, and reply status.
func TestRequestKind(t *testing.T) {
	req := func() Envelope {
		return Envelope{Kind: KindRequest, To: []string{"@archie"}, Subject: "Land 0002", Body: "Please land it.", Deliverable: "Merged PR and deploy event"}
	}
	e := req()
	e.Normalize()
	if _, err := Check(e, dir, nil); err != nil {
		t.Fatalf("valid request refused: %v", err)
	}
	if TransportKind(KindRequest) != "request" {
		t.Fatalf("request transport kind %q", TransportKind(KindRequest))
	}
	cases := []struct {
		name  string
		edit  func(*Envelope)
		field string
	}{
		{"no deliverable", func(e *Envelope) { e.Deliverable = "" }, "deliverable"},
		{"options", func(e *Envelope) { e.Options = []Option{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}} }, "options"},
		{"no subject", func(e *Envelope) { e.Subject = "" }, "subject"},
		{"status without in_reply_to", func(e *Envelope) { e.Status = StatusDone }, "status"},
		{"unknown status", func(e *Envelope) { e.InReplyTo = "m1"; e.Status = "finished" }, "status"},
		{"deliverable on a non-request", func(e *Envelope) { e.Kind = KindUpdate }, "deliverable"},
		{"multi-line evidence", func(e *Envelope) { e.Evidence = []string{"a\nb"} }, "evidence[0]"},
	}
	for _, c := range cases {
		e := req()
		c.edit(&e)
		e.Normalize()
		if _, err := Check(e, dir, nil); err == nil || fieldOf(t, err) != c.field {
			t.Fatalf("%s: err %v, want field %s", c.name, err, c.field)
		}
	}
	reply := Envelope{Kind: KindMessage, To: []string{"@archie"}, InReplyTo: "m1", Body: "Done.", Status: StatusDone, Evidence: []string{"https://github.com/hollis-labs/tether/pull/606"}}
	reply.Normalize()
	if err := reply.Validate(); err != nil {
		t.Fatalf("reply with status refused: %v", err)
	}
}

// A question without a subject still passes: it defaults to the body's first line.
func TestLegacyKindsDefaultTheirSubject(t *testing.T) {
	e := Envelope{Kind: KindQuestion, To: []string{"@archie"}, Body: "Which one?\nmore", Options: []Option{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}}
	e.Normalize()
	if e.Subject != "Which one?" {
		t.Fatalf("subject %q", e.Subject)
	}
}
