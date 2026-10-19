package environmentstream

import (
	"github.com/hollis-labs/tether/internal/store"
	"testing"
)

func TestRequestOrderingFencesUnknownAndExactTurn(t *testing.T) {
	for _, equal := range []bool{false, true} {
		s := &Session{}
		applyRequest(s, store.EnvironmentRequest{TurnID: "t", RequestID: "one", SourceSequence: 20})
		applyRequest(s, store.EnvironmentRequest{Terminal: true, TurnID: "ended", SourceSequence: 30})
		if s.ApprovalKnown || s.QuestionKnown {
			t.Fatal("unknown resolution invented support")
		}
		seq := uint64(19)
		if equal {
			seq = 20
		}
		applyRequest(s, store.EnvironmentRequest{TurnID: "t", RequestID: "one", Kind: "approval", Open: true, SourceSequence: seq})
		if !s.ApprovalKnown || s.PendingApproval {
			t.Fatal("late open bypassed resolution", s)
		}
		applyRequest(s, store.EnvironmentRequest{TurnID: "ended", RequestID: "two", Kind: "question", Open: true, SourceSequence: 29})
		if !s.QuestionKnown || s.PendingQuestion {
			t.Fatal("late open bypassed terminal", s)
		}
		applyRequest(s, store.EnvironmentRequest{TurnID: "other", RequestID: "two", Kind: "question", Open: true, SourceSequence: 29})
		if !s.PendingQuestion {
			t.Fatal("other turn was closed")
		}
	}
}
