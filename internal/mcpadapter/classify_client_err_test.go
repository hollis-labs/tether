package mcpadapter

import (
	"errors"
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
