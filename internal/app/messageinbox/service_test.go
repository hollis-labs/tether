package messageinbox

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/store"
)

type testStore struct {
	unread   bool
	result   []messaging.Envelope
	err      error
	consumed []string
	filter   messaging.Filter
	to       messaging.Address
}

func (s *testStore) Inbox(ctx context.Context, to messaging.Address, f messaging.Filter) ([]messaging.Envelope, error) {
	s.to, s.filter, s.unread = to, f, store.InboxUnreadOnly(ctx)
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

// The session lookup now precedes the pull (it decides whether the pull is unread-only),
// so a failed pull still never consumes anything.
func TestPull_InboxFailureSkipsConsume(t *testing.T) {
	want := errors.New("inbox unavailable")
	st := &testStore{err: want}
	sessions := &testSessions{}
	got, err := New(st, sessions).Pull(context.Background(), messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}, messaging.Filter{}, "live-session")
	if got.Messages != nil || !errors.Is(err, want) || len(st.consumed) != 0 {
		t.Fatalf("failed pull performed follow-up work: %v, %v, consumed=%v", got, err, st.consumed)
	}
}

// CW-20261021-0029: the pull a live recipient session makes is unread-only; an operator
// pull, or a session that is not the recipient, is the plain pull.
func TestPull_OnlyTheRecipientSessionPullsUnreadOnly(t *testing.T) {
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	for name, tc := range map[string]struct {
		sessionID string
		to        messaging.Address
		unread    bool
	}{
		"live recipient session":       {"live-session", to, true},
		"operator pull (no session)":   {"", to, false},
		"session of another recipient": {"live-session", messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "someone-else"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			st := &testStore{}
			if _, err := New(st, &testSessions{}).Pull(context.Background(), tc.to, messaging.Filter{}, tc.sessionID); err != nil {
				t.Fatal(err)
			}
			if st.unread != tc.unread {
				t.Fatalf("unread-only = %v, want %v", st.unread, tc.unread)
			}
		})
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
