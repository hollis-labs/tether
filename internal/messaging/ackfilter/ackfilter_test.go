package ackfilter

import "testing"

func TestIsAck(t *testing.T) {
	for _, s := range []string{"Noted. No reply needed.", "Thanks", "ack", "Ack.", "Thanks, noted!", "Got it - thank you", "OK", "Understood. No action needed.", "Noted, thanks for the update."} {
		if !IsAck(s) {
			t.Errorf("%q should be an ack", s)
		}
	}
	for _, s := range []string{"", "   ", "Thanks, can you also add the test?", "Noted. I will merge PR #12 tonight", "ack?", "Done. PR merged", "ok but the build fails", "No", "fine", "and", "Noted: deploy blocked on review", "Thanks. One more thing: the key rotated", "great, now do X",
		"Noted. No reply needed. Noted. No reply needed. Noted. No reply needed."} {
		if IsAck(s) {
			t.Errorf("%q must not be an ack", s)
		}
	}
}
