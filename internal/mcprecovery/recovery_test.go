package mcprecovery

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
	"time"
)

func TestBackoffAndRecoveryBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var attempts []time.Duration
		err := Run(context.Background(), func(context.Context) (bool, error) {
			attempts = append(attempts, time.Since(start))
			return true, errors.New("connection refused")
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		want := []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second, 15 * time.Second, 31 * time.Second}
		if !reflect.DeepEqual(attempts, want) {
			t.Fatalf("attempt times %v, want %v", attempts, want)
		}
		if time.Since(start) != Budget {
			t.Fatalf("budget exceeded: %v", time.Since(start))
		}
	})
}

func TestPermanentFailureAndCancellation(t *testing.T) {
	permanent := errors.New("admission refused")
	calls := 0
	err := Run(context.Background(), func(context.Context) (bool, error) { calls++; return false, permanent })
	if !errors.Is(err, permanent) || calls != 1 {
		t.Fatalf("%v / %d", err, calls)
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(500*time.Millisecond, cancel)
		err := Run(ctx, func(context.Context) (bool, error) { return true, errors.New("offline") })
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}
