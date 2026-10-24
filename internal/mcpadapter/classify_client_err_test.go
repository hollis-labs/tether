package mcpadapter

import (
	"errors"
	"strings"
	"testing"
)

// classifyClientErr keys on the "(code)" readError writes, so a lost
// provider resume id keeps its own code instead of folding into the 409
// conflict case.
func TestClassifyClientErr(t *testing.T) {
	cases := []struct {
		msg, want string
	}{
		{`daemon 409 (provider_session_lost): agentsessions: provider session "ses_dead": provider session lost: exit 1`, "provider_session_lost"},
		{"daemon 409 (conflict): session has no input channel", "conflict"},
		{"daemon 404 (not_found): session not running", "not_found"},
		{"daemon 400 (invalid_request): decode body", "invalid_request"},
		{"daemon 502 (turn_failed): turn failed: runner: process exited 1", "turn_failed"},
		{"daemon 500 (internal_error): boom", "internal_error"},
	}
	for _, tc := range cases {
		if got := classifyClientErr(errors.New(tc.msg), "").Code; got != tc.want {
			t.Errorf("%q → %q; want %q", tc.msg, got, tc.want)
		}
	}
}

func TestClassifyClientErr_IdempotencyConflict(t *testing.T) {
	msg := `daemon 409 (idempotency_conflict): idempotency key was already used with a different request (key bound to session s1 by a create request)`
	if got := classifyClientErr(errors.New(msg), "").Code; got != "idempotency_conflict" {
		t.Fatalf("code = %q; want idempotency_conflict", got)
	}
}

// tether_message_reply only answers a routed session message. For ordinary mail
// the redirect must be typed (HelpTool/NextStep), not only prose (CW-20261024-0051).
func TestReplyToMailboxMessageCarriesATypedRedirect(t *testing.T) {
	err := errors.New("daemon 400 (reply_target_not_a_session): the message was not sent by a session")
	got := classifyClientErr(err, "")
	if got.Code != "invalid_request" {
		t.Fatalf("code = %q", got.Code)
	}
	if got.HelpTool != "tether_message_send" || !strings.Contains(got.NextStep, "in_reply_to") {
		t.Fatalf("redirect not typed: helpTool=%q nextStep=%q", got.HelpTool, got.NextStep)
	}
	// Other 400s are unchanged and carry no redirect.
	other := classifyClientErr(errors.New("daemon 400 (invalid_request): bad thing"), "")
	if other.HelpTool != "" || other.NextStep != "" {
		t.Fatalf("an unrelated 400 must not get a redirect: %+v", other)
	}
}

// A send to a retired actor is refused with its own code and a typed pointer to
// the registry, so the sender can find the replacement (CW-20261024-0095).
func TestSendToDeprecatedRecipientCarriesATypedRedirect(t *testing.T) {
	err := errors.New("daemon 409 (recipient_deprecated): recipient is deprecated: msg://agent/agent-mux/agt_old was deprecated in the registry; nothing was sent")
	got := classifyClientErr(err, "")
	if got.Code != "recipient_deprecated" {
		t.Fatalf("code = %q", got.Code)
	}
	if got.HelpTool != "tether_registry_search" || !strings.Contains(got.NextStep, "tether_registry_lookup_by") {
		t.Fatalf("redirect not typed: helpTool=%q nextStep=%q", got.HelpTool, got.NextStep)
	}
}
