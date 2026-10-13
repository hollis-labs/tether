package workerruntime

import (
	"testing"
	"time"
)

func TestReconnectBackoffScheduleAndJitter(t *testing.T) {
	mid := func() float64 { return 0.5 }
	b := newReconnectBackoff(nil, mid)
	want := []time.Duration{1, 2, 4, 8, 15, 15}
	for i, w := range want {
		if got := b.next(); got != w*time.Second {
			t.Fatalf("step %d: got %s want %ds", i, got, w)
		}
	}
	lo := newReconnectBackoff(nil, func() float64 { return 0 })
	hi := newReconnectBackoff(nil, func() float64 { return 1 })
	if got := lo.next(); got != 800*time.Millisecond {
		t.Fatalf("low jitter: %s", got)
	}
	if got := hi.next(); got != 1200*time.Millisecond {
		t.Fatalf("high jitter: %s", got)
	}
}

func TestReconnectBackoffResetsOnlyAfterStablePeriod(t *testing.T) {
	now := time.Unix(0, 0)
	b := newReconnectBackoff(func() time.Time { return now }, func() float64 { return 0.5 })
	b.next()
	b.next() // cur is now 4s
	up := now
	now = now.Add(5 * time.Second) // flapping: dropped shortly after welcome
	b.connectionEnded(now.Sub(up))
	if got := b.next(); got != 4*time.Second {
		t.Fatalf("flap must not reset: got %s", got)
	}
	up = now
	now = now.Add(reconnectStable) // stayed up
	b.connectionEnded(now.Sub(up))
	if got := b.next(); got != time.Second {
		t.Fatalf("stable connection must reset: got %s", got)
	}
}
