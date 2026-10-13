package app

import (
	"testing"
	"time"
)

func TestClassifyStall(t *testing.T) {
	cases := []struct {
		name    string
		denied  int
		idle    time.Duration
		stalled bool
		reason  string
	}{
		{"healthy", 0, time.Hour, false, ""},
		{"one denial recent", 1, time.Minute, false, ""},
		{"threshold denials", 3, 0, true, stallReasonDenials},
		{"denied then idle", 1, 11 * time.Minute, true, stallReasonDeniedAndIdle},
	}
	for _, c := range cases {
		st := classifyStall(c.denied, c.idle)
		if st.Stalled != c.stalled || st.Reason != c.reason || st.DeniedSinceOutput != c.denied {
			t.Errorf("%s: got %+v", c.name, st)
		}
	}
}
