package app

import (
	"context"
	"time"
)

type interruptClock interface {
	Now() time.Time
	Wait(context.Context, time.Duration) error
}

type realInterruptClock struct{}

func (realInterruptClock) Now() time.Time { return time.Now() }
func (realInterruptClock) Wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
