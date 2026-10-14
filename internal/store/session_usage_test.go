package store

import (
	"testing"
	"time"
)

func TestSessionUsageAccumulates(t *testing.T) {
	s := openTestStoreForSessions(t)
	u, err := s.GetSessionUsage("none")
	if err != nil || u.Turns != 0 || u.UsageTurns != 0 {
		t.Fatalf("empty meter %+v %v", u, err)
	}
	for _, tu := range []TurnUsage{
		{Reported: true, InputTokens: 10, OutputTokens: 5, CostUSD: 0.25, Duration: time.Second},
		{Failed: true, Duration: 500 * time.Millisecond},
	} {
		if err := s.AddSessionTurnUsage("a", tu); err != nil {
			t.Fatal(err)
		}
	}
	u, _ = s.GetSessionUsage("a")
	if u.Turns != 2 || u.FailedTurns != 1 || u.UsageTurns != 1 || u.InputTokens != 10 || u.OutputTokens != 5 || u.CostUSD != 0.25 || u.TurnMS != 1500 {
		t.Fatalf("meter %+v", u)
	}
}
