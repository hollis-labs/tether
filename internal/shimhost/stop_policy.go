//go:build !windows

package shimhost

import (
	"context"
	"fmt"
	"strconv"
	"syscall"
	"time"
)

func stopProviderInTiers(ctx context.Context, c *Client, session string, generation uint64, p StopPolicy) (bool, error) {
	for _, tier := range []struct {
		name   string
		signal syscall.Signal
		grace  time.Duration
	}{
		{"request_stop", syscall.SIGINT, p.RequestGrace}, {"terminate", syscall.SIGTERM, p.TerminateGrace}, {"kill", syscall.SIGKILL, p.KillGrace},
	} {
		running, _, _, err := health(ctx, c, session)
		if err != nil || !running {
			return running, err
		}
		if p.BeforeStage != nil {
			if err := p.BeforeStage(ctx, tier.name); err != nil {
				return true, err
			}
		}
		if err = c.Send(session, "control", map[string]any{"action": "signal", "signal": int(tier.signal), "expected_generation": strconv.FormatUint(generation, 10)}); err != nil {
			return true, err
		}
		deadline := time.NewTimer(tier.grace)
		ticker := time.NewTicker(20 * time.Millisecond)
		waiting := true
		for waiting {
			running, _, _, err = health(ctx, c, session)
			if err != nil || !running {
				deadline.Stop()
				ticker.Stop()
				return running, err
			}
			select {
			case <-ctx.Done():
				deadline.Stop()
				ticker.Stop()
				return true, ctx.Err()
			case <-deadline.C:
				waiting = false
			case <-ticker.C:
			}
		}
		deadline.Stop()
		ticker.Stop()
	}
	return true, fmt.Errorf("provider still running after kill grace; placement retained")
}
