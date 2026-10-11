package teamloop

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func wait(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRunsBothWithLimit(t *testing.T) {
	var flush, rec, lim atomic.Int32
	stop := Start(context.Background(), Config{Interval: time.Millisecond, Limit: 7,
		Flush:     func(_ context.Context, l int) error { flush.Add(1); lim.Store(int32(l)); return nil },
		Reconcile: func(context.Context, int) error { rec.Add(1); return nil }})
	defer stop()
	wait(t, func() bool { return flush.Load() > 1 && rec.Load() > 1 })
	if lim.Load() != 7 {
		t.Fatal("limit not passed", lim.Load())
	}
}

func TestNoOverlapAndStopWaits(t *testing.T) {
	var running, peak, calls atomic.Int32
	slow := func(context.Context, int) error {
		n := running.Add(1)
		if n > peak.Load() {
			peak.Store(n)
		}
		time.Sleep(5 * time.Millisecond)
		running.Add(-1)
		calls.Add(1)
		return nil
	}
	stop := Start(context.Background(), Config{Interval: time.Millisecond, Flush: slow, Reconcile: slow})
	wait(t, func() bool { return calls.Load() > 4 })
	stop()
	if running.Load() != 0 {
		t.Fatal("stop returned with work in flight")
	}
	if peak.Load() != 1 {
		t.Fatal("overlap", peak.Load())
	}
	n := calls.Load()
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != n {
		t.Fatal("ran after stop")
	}
	stop()
}

func TestStopsOnParentCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	stop := Start(ctx, Config{Interval: time.Millisecond, Flush: func(context.Context, int) error { calls.Add(1); return nil }})
	wait(t, func() bool { return calls.Load() > 0 })
	cancel()
	stop()
	n := calls.Load()
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != n {
		t.Fatal("ran after cancel")
	}
}

func TestSurvivesErrorAndPanic(t *testing.T) {
	var flush, rec, logs atomic.Int32
	stop := Start(context.Background(), Config{Interval: time.Millisecond,
		Flush:     func(context.Context, int) error { flush.Add(1); return errors.New("boom") },
		Reconcile: func(context.Context, int) error { rec.Add(1); panic("bad") },
		Logf:      func(string, ...any) { logs.Add(1) }})
	defer stop()
	wait(t, func() bool { return flush.Load() > 2 && rec.Load() > 2 && logs.Load() > 4 })
}
