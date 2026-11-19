package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func operatorReplyStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOperatorReplyChallengesAreCappedAndConsumedOnce(t *testing.T) {
	s := operatorReplyStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := range 10 {
		c := OperatorReplyChallenge{ID: fmt.Sprintf("orc_%d", i), NonceSHA256: "n", MessageID: "m", Option: "approve", Binding: "b", BodySHA256: "h", DeviceID: "dev", CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute)}
		if err := s.CreateOperatorReplyChallenge(ctx, c, 10); err != nil {
			t.Fatal(err)
		}
	}
	over := OperatorReplyChallenge{ID: "orc_x", DeviceID: "dev", CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	if err := s.CreateOperatorReplyChallenge(ctx, over, 10); !errors.Is(err, ErrTooManyChallenges) {
		t.Fatalf("11th open challenge: %v", err)
	}
	other := over
	other.ID, other.DeviceID = "orc_y", "dev2"
	if err := s.CreateOperatorReplyChallenge(ctx, other, 10); err != nil {
		t.Fatalf("another device's challenge: %v", err)
	}
	if ok, err := s.ConsumeOperatorReplyChallenge(ctx, "orc_0", now); !ok || err != nil {
		t.Fatalf("first consume: %v %v", ok, err)
	}
	if ok, _ := s.ConsumeOperatorReplyChallenge(ctx, "orc_0", now); ok {
		t.Fatal("a challenge was consumed twice")
	}
	if c, err := s.OperatorReplyChallenge(ctx, "orc_0"); err != nil || c.ConsumedAt == nil {
		t.Fatalf("consumed challenge: %+v %v", c, err)
	}
	// A consumed challenge frees a slot.
	if err := s.CreateOperatorReplyChallenge(ctx, over, 10); err != nil {
		t.Fatalf("after a consume: %v", err)
	}
	if _, err := s.OperatorReplyChallenge(ctx, "nope"); !errors.Is(err, ErrChallengeUnknown) {
		t.Fatal(err)
	}
}

// The reply-time transaction is the real guard: of many concurrent replies to
// one parent exactly one wins; a spent jti or elevation_id is a replay.
func TestReserveOperatorReplyIsTheConcurrentGuard(t *testing.T) {
	s := operatorReplyStore(t)
	ctx := context.Background()
	forms := []string{"msg://user/local/operator"}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, answered := 0, 0
	for i := range 20 {
		wg.Go(func() {
			err := s.ReserveOperatorReply(ctx, OperatorReplyReservation{MessageID: "parent", ChallengeID: fmt.Sprintf("orc_%d", i), JTI: fmt.Sprintf("jti-%d", i), ElevationID: fmt.Sprintf("el-%d", i), At: time.Now()}, forms)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrAlreadyAnswered):
				answered++
			default:
				t.Errorf("reserve: %v", err)
			}
		})
	}
	wg.Wait()
	if wins != 1 || answered != 19 {
		t.Fatalf("wins %d, already answered %d", wins, answered)
	}
	if ok, _ := s.OperatorAnswered(ctx, "parent", forms); !ok {
		t.Fatal("a reserved parent is not answered")
	}
	for name, r := range map[string]OperatorReplyReservation{
		"jti reused":          {MessageID: "p2", ChallengeID: "c", JTI: "jti-0", ElevationID: "fresh-1", At: time.Now()},
		"elevation_id reused": {MessageID: "p3", ChallengeID: "c", JTI: "fresh-2", ElevationID: "el-0", At: time.Now()},
	} {
		// el-0/jti-0 were spent only if that goroutine won; spend them now to be sure.
		_ = s.ReserveOperatorReply(ctx, OperatorReplyReservation{MessageID: "seed-" + name, ChallengeID: "c", JTI: "jti-0", ElevationID: "el-0", At: time.Now()}, forms)
		if err := s.ReserveOperatorReply(ctx, r, forms); !errors.Is(err, ErrReplayed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A reservation whose reply could not be stored is released; the proofs stay spent.
	if err := s.ReserveOperatorReply(ctx, OperatorReplyReservation{MessageID: "p4", ChallengeID: "c4", JTI: "jti-4x", ElevationID: "el-4x", At: time.Now()}, forms); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseOperatorReply(ctx, "p4"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.OperatorAnswered(ctx, "p4", forms); ok {
		t.Fatal("released reservation still answers")
	}
	if used, _ := s.OperatorReplyProofUsed(ctx, "jti-4x", "x"); !used {
		t.Fatal("the released reply's jti is reusable")
	}
}

func TestOperatorAttestationTrustRecord(t *testing.T) {
	s := operatorReplyStore(t)
	ctx := context.Background()
	if _, ok, err := s.ApprovedOperatorAttestation(ctx); ok || err != nil {
		t.Fatalf("fresh store: %v %v", ok, err)
	}
	prev, had, err := s.SetApprovedOperatorAttestation(ctx, OperatorAttestationTrust{Digest: "d1", KeyIDs: []string{"k1"}, PlanID: "p1", ApprovedAt: time.Now()})
	if err != nil || had {
		t.Fatalf("first set: %v %v", had, err)
	}
	if _, had, _ = s.SetApprovedOperatorAttestation(ctx, OperatorAttestationTrust{Digest: "d2", KeyIDs: []string{"k1", "k2"}, PlanID: "p2", ApprovedAt: time.Now()}); !had {
		t.Fatal("second set saw no previous record")
	}
	if got, _, _ := s.ApprovedOperatorAttestation(ctx); got.Digest != "d2" || len(got.KeyIDs) != 2 {
		t.Fatalf("record %+v", got)
	}
	if err = s.RestoreOperatorAttestation(ctx, prev, false); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.ApprovedOperatorAttestation(ctx); ok {
		t.Fatal("restore to none kept a record")
	}
}
