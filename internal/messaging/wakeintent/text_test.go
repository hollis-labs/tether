package wakeintent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/messaging/envelope"
)

func TestDefaultWakeTextIsAPointerThatEndsEmpty(t *testing.T) {
	env := messaging.Envelope{
		ID:   "m1",
		From: messaging.Address{Kind: messaging.KindAgent, Authority: "agent-mux", ID: "archie"},
		To:   messaging.Address{Kind: messaging.KindAgent, Authority: "agent-mux", ID: "photon-tether"},
		Kind: messaging.MsgKindNotice,
	}
	got := DefaultWakeText(env, 2, "high", false)
	for _, want := range []string{"**Mailbox wake (daemon-injected)**", "2 unread message(s)", "`m1`", "urgency `high`", "Reply to anything that asked you something", "If nothing needs action, end the turn without producing any message"} {
		if !strings.Contains(got, want) {
			t.Fatalf("wake text missing %q:\n%s", want, got)
		}
	}
	// The old text asked for an inbox-check procedure and checklist, which produced status chatter.
	for _, gone := range []string{"inbox-check procedure", "checklist"} {
		if strings.Contains(got, gone) {
			t.Fatalf("wake text still contains %q:\n%s", gone, got)
		}
	}

	retried := DefaultWakeText(env, 0, "normal", true)
	if !strings.Contains(retried, "(daemon-injected, retried)") || strings.Contains(retried, "message(s)") || !strings.Contains(retried, WakeGuidance) {
		t.Fatalf("retried wake text = %q", retried)
	}
}

// CW-20261114-0116: a wake for mail an assistant sent on someone's behalf says so, so the woken seat never
// reads it as that person's own instruction.
func TestWakeTextShowsOnBehalfOf(t *testing.T) {
	from, _ := messaging.ParseURN("msg://agent/agent-mux/agt_hg8p0txi1y")
	to, _ := messaging.ParseURN("msg://agent/agent-mux/agt_worker")
	env := messaging.Envelope{ID: "m1", Kind: messaging.MsgKindNotice, From: from, To: to, ContentType: envelope.ContentType,
		Payload: []byte(`{"kind":"message","to":["@worker"],"body":"x","on_behalf_of":"@chrispian","drafted_by":"assistant"}`)}
	text := DefaultWakeText(env, 1, "normal", false)
	if !strings.Contains(text, "Sent on behalf of `@chrispian` (drafted by assistant): it is the sender's, not @chrispian's own instruction.") {
		t.Fatalf("wake text: %s", text)
	}
	env.ContentType = ""
	if strings.Contains(DefaultWakeText(env, 1, "normal", false), "on behalf of") {
		t.Fatal("untyped mail has no marker")
	}
}

// The wake never tells an agent to end silently without the "if nothing needs
// action" condition (CW-20261117-0002).
func TestWakeGuidanceNeverUnconditionallySilent(t *testing.T) {
	if strings.Contains(WakeGuidance, "end the turn silently") {
		t.Fatalf("guidance still carries the unconditional silent-end instruction: %q", WakeGuidance)
	}
	if strings.Contains(strings.ToLower(WakeGuidance), "silently") {
		t.Fatalf("guidance still says silently: %q", WakeGuidance)
	}
	if !strings.Contains(WakeGuidance, "Reply to anything that asked you something") {
		t.Fatalf("guidance lost the reply instruction: %q", WakeGuidance)
	}
	if !strings.Contains(WakeGuidance, "If nothing needs action, end the turn without producing any message") {
		t.Fatalf("guidance lost the conditional: %q", WakeGuidance)
	}
}

// A typed ask's wake names the sender handle, subject and deliverable (CW-20261117-0003).
func TestWakeTextSummarisesATypedRequest(t *testing.T) {
	raw, _ := json.Marshal(envelope.Envelope{Schema: envelope.Schema, Kind: envelope.KindRequest, From: "@archie", To: []string{"@tether"}, Subject: "Land 0002", Body: "please", Deliverable: "Merged PR"})
	from, _ := messaging.ParseURN("msg://agent/agent-mux/agt_archie")
	to, _ := messaging.ParseURN("msg://agent/agent-mux/agt_me")
	text := DefaultWakeText(messaging.Envelope{ID: "m9", From: from, To: to, Kind: messaging.MsgKindRequest, ContentType: envelope.ContentType, Payload: raw}, 1, "normal", false)
	for _, want := range []string{"@archie request: Land 0002", "Deliverable: Merged PR", "tether_reply"} {
		if !strings.Contains(text, want) {
			t.Fatalf("wake text lacks %q:\n%s", want, text)
		}
	}
}
