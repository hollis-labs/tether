package store

import (
	"context"
	"testing"

	"github.com/hollis-labs/tether/internal/events"
)

func TestSessionActivityCountsDenialsSinceOutput(t *testing.T) {
	s := openTestStoreForSessions(t)
	ctx := context.Background()
	ins := func(kind string) {
		t.Helper()
		if _, _, err := s.InsertEventContext(ctx, events.ScopeSession, "s1", kind, ""); err != nil {
			t.Fatal(err)
		}
	}
	ins(events.KindProviderPermissionDenied)
	ins(events.KindSessionTurnOutput)
	ins(events.KindProviderPermissionDenied)
	ins(events.KindProviderPermissionDenied)

	act, err := s.SessionActivity(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if act.DeniedSinceOutput != 2 || act.LastEventAt.IsZero() {
		t.Fatalf("activity = %+v", act)
	}
	none, err := s.SessionActivity(ctx, "other")
	if err != nil || none.DeniedSinceOutput != 0 || !none.LastEventAt.IsZero() {
		t.Fatalf("empty activity = %+v, %v", none, err)
	}
}
