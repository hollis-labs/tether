package app

import (
	"context"
	"errors"
	gopevents "github.com/hollis-labs/go-providers/provider/events"
	"testing"
	"time"
)

type fakeInterruptClock struct {
	now   time.Time
	waits int
	tick  func()
}

func (c *fakeInterruptClock) Now() time.Time { return c.now }
func (c *fakeInterruptClock) Wait(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.now = c.now.Add(delay)
	c.waits++
	if c.tick != nil {
		c.tick()
	}
	return ctx.Err()
}

func TestCancelTurnReadinessGap(t *testing.T) {
	for _, readyAfter := range []int{3, -1} {
		t.Run(time.Duration(readyAfter).String(), func(t *testing.T) {
			var state *sessionTurnOutput
			calls := 0
			svc, output, session := interruptHarness(t, func(context.Context) error {
				calls++
				state.observeProvider(gopevents.Done{})
				return nil
			})
			state = output
			ready := false
			session.readiness = func() (bool, error) { return ready, nil }
			if err := svc.SendInput("s1", []byte("start")); err != nil {
				t.Fatal(err)
			}
			id, _ := state.CurrentTurn()
			clock := &fakeInterruptClock{now: time.Unix(0, 0)}
			clock.tick = func() {
				if clock.waits == readyAfter {
					ready = true
				}
			}
			result, err := svc.cancelTurnAndWaitWithClock(interruptTestContext(t), "s1", "msg://user/local/alice", state, id, clock)
			if readyAfter > 0 {
				if err != nil || calls != 1 || clock.waits != readyAfter || result.OutputTurnID != id {
					t.Fatalf("result=%+v err=%v calls=%d waits=%d", result, err, calls, clock.waits)
				}
			} else {
				requireRefusal(t, err, TurnInterruptNotStarted)
				if calls != 0 || clock.now.Sub(time.Unix(0, 0)) != 2*time.Second {
					t.Fatalf("calls=%d elapsed=%s", calls, clock.now.Sub(time.Unix(0, 0)))
				}
				current, _ := state.CurrentTurn()
				if current != id {
					t.Fatal("refusal settled a live turn")
				}
			}
		})
	}
}

func TestCancelTurnReadinessWaitContextAndCompletion(t *testing.T) {
	for _, action := range []string{"context", "terminal"} {
		t.Run(action, func(t *testing.T) {
			calls := 0
			svc, state, session := interruptHarness(t, func(context.Context) error { calls++; return nil })
			session.readiness = func() (bool, error) { return false, nil }
			if err := svc.SendInput("s1", []byte("start")); err != nil {
				t.Fatal(err)
			}
			id, _ := state.CurrentTurn()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clock := &fakeInterruptClock{}
			clock.tick = func() {
				if action == "context" {
					cancel()
				} else {
					state.observeProvider(gopevents.Done{})
				}
			}
			_, err := svc.cancelTurnAndWaitWithClock(ctx, "s1", "msg://user/local/alice", state, id, clock)
			if action == "context" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error=%v", err)
				}
			} else {
				requireRefusal(t, err, TurnInterruptNoTurn)
			}
			if calls != 0 || clock.waits != 1 {
				t.Fatalf("calls=%d waits=%d", calls, clock.waits)
			}
		})
	}
}
