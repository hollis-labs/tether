package teamstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/teamstore"
)

func TestRecoveryBackoffCapsAndSurvivesClockRollback(t *testing.T) {
	ctx := context.Background()
	_, s := open(t, t.TempDir()+"/db", teamstore.Options{})
	now := time.Now()
	failure := errors.New("offline")
	for i := 0; i < 12; i++ {
		reported := s.FinishRecovery(ctx, "session", "key", "launch", now, failure)
		if i == 0 && !errors.Is(reported, failure) {
			t.Fatal(reported)
		}
		if i > 0 && reported != nil {
			t.Fatal("repeated error logged", reported)
		}
		ready, err := s.RecoveryReady(ctx, "session", "key", "launch", now)
		must(t, err)
		if ready {
			t.Fatal("failure immediately retried")
		}
		now = now.Add(time.Minute)
		ready, err = s.RecoveryReady(ctx, "session", "key", "launch", now)
		must(t, err)
		if !ready {
			t.Fatal("retry exceeded cap")
		}
	}
	// Rolling the clock back persists a clamp instead of pushing the retry
	// forward on every read. Cleanup changes remain immediately eligible.
	past := now.Add(-time.Hour)
	ready, err := s.RecoveryReady(ctx, "session", "key", "launch", past)
	must(t, err)
	if ready {
		t.Fatal("rollback lost backoff")
	}
	ready, err = s.RecoveryReady(ctx, "session", "key", "stop", past)
	must(t, err)
	if !ready {
		t.Fatal("cleanup held by acquisition delay")
	}
	ready, err = s.RecoveryReady(ctx, "session", "key", "launch", past.Add(time.Minute))
	must(t, err)
	if !ready {
		t.Fatal("clock clamp moved")
	}
}
