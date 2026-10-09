package messageinbox

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/hollis-labs/substrate/mesh/messaging"
)

type testStore struct {
	result   []messaging.Envelope
	err      error
	consumed []string
	filter   messaging.Filter
	to       messaging.Address
}

func (s *testStore) Inbox(_ context.Context, to messaging.Address, f messaging.Filter) ([]messaging.Envelope, error) {
	s.to, s.filter = to, f
	return s.result, s.err
}

func (s *testStore) Consume(_ context.Context, id string, _ messaging.Address) error {
	s.consumed = append(s.consumed, id)
	return errors.New("consume unavailable")
}

type testSessions struct{ lookups int }

func (s *testSessions) Live(string) bool                      { s.lookups++; return true }
func (s *testSessions) LogicalAgentID(string) (string, error) { return "worker", nil }

func TestPull_ConsumeFailuresPreserveThePullAndContinue(t *testing.T) {
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	filter := messaging.Filter{Kind: []messaging.Kind{messaging.MsgKindNotice}, ThreadID: "thread"}
	st := &testStore{result: []messaging.Envelope{{ID: "first"}, {ID: "second"}}}
	got, err := New(st, &testSessions{}).Pull(context.Background(), to, filter, "live-session")
	if err != nil || !reflect.DeepEqual(got.Messages, st.result) {
		t.Fatalf("successful pull changed after consume errors: %v, %v", got, err)
	}
	if !reflect.DeepEqual(st.consumed, []string{"first", "second"}) {
		t.Fatalf("consume failure stopped later attempts: %v", st.consumed)
	}
	if st.to != to || !reflect.DeepEqual(st.filter, filter) {
		t.Fatalf("pull address/filter changed: %v, %v", st.to, st.filter)
	}
}

func TestPull_InboxFailureSkipsSessionLookupAndConsume(t *testing.T) {
	want := errors.New("inbox unavailable")
	st := &testStore{err: want}
	sessions := &testSessions{}
	got, err := New(st, sessions).Pull(context.Background(), messaging.Address{}, messaging.Filter{}, "live-session")
	if got.Messages != nil || !errors.Is(err, want) || sessions.lookups != 0 || len(st.consumed) != 0 {
		t.Fatalf("failed pull performed follow-up work: %v, %v, lookups=%d, consumed=%v", got, err, sessions.lookups, st.consumed)
	}
}

func TestPull_OperatorPullPreservesNilResultWithoutSessionLookup(t *testing.T) {
	st := &testStore{}
	sessions := &testSessions{}
	got, err := New(st, sessions).Pull(context.Background(), messaging.Address{}, messaging.Filter{}, "")
	if got.Messages != nil || err != nil || sessions.lookups != 0 || len(st.consumed) != 0 {
		t.Fatalf("plain pull changed: %v, %v, lookups=%d, consumed=%v", got, err, sessions.lookups, st.consumed)
	}
}
