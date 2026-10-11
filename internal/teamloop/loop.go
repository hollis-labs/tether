// Package teamloop is the daemon-owned background tick that drives team
// message flushing and port reconciliation. It has no team dependencies so
// the host and runtime stay unaware of scheduling.
package teamloop

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	DefaultInterval = 5 * time.Second
	DefaultLimit    = 50
)

// Func is one bounded unit of work, e.g. Host.FlushMessages or Reconciler.Reconcile.
type Func func(ctx context.Context, limit int) error

// Config describes the loop. Zero Interval and Limit use the defaults.
type Config struct {
	Interval  time.Duration
	Limit     int
	Flush     Func
	Reconcile Func
	// Logf receives only errors and panics.
	Logf func(format string, args ...any)
}

// Start runs the loop until ctx is canceled or the returned stop is called.
// Ticks run sequentially on one goroutine, so they never overlap. stop cancels
// and waits for any in-flight tick to return; it is safe to call repeatedly.
func Start(ctx context.Context, cfg Config) (stop func()) {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Limit < 1 {
		cfg.Limit = DefaultLimit
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				tick(ctx, cfg)
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(cancel)
		<-done
	}
}

func tick(ctx context.Context, cfg Config) {
	for _, step := range []struct {
		name string
		fn   Func
	}{{"flush messages", cfg.Flush}, {"reconcile", cfg.Reconcile}} {
		if step.fn == nil || ctx.Err() != nil {
			continue
		}
		if err := safe(ctx, step.fn, cfg.Limit); err != nil && ctx.Err() == nil {
			cfg.Logf("teams: %s: %v", step.name, err)
		}
	}
}

func safe(ctx context.Context, fn Func, limit int) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn(ctx, limit)
}
