package wakeintent

import (
	"strings"
	"testing"

	"github.com/hollis-labs/libs/mesh/core/messaging"
)

func TestDefaultWakeTextIsAPointerThatEndsSilently(t *testing.T) {
	env := messaging.Envelope{
		ID:   "m1",
		From: messaging.Address{Kind: messaging.KindAgent, Authority: "agent-mux", ID: "archie"},
		To:   messaging.Address{Kind: messaging.KindAgent, Authority: "agent-mux", ID: "photon-tether"},
		Kind: messaging.MsgKindNotice,
	}
	got := DefaultWakeText(env, 2, "high", false)
	for _, want := range []string{"**Mailbox wake (daemon-injected)**", "2 unread message(s)", "`m1`", "urgency `high`", "mark it read and end the turn silently"} {
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
