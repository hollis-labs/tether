package workerruntime

import (
	"context"
	"log"
	"path/filepath"
	"time"

	"github.com/hollis-labs/tether/internal/devicepair"
	"github.com/hollis-labs/tether/internal/identity"
)

const deviceRenewInterval = 24 * time.Hour
const deviceRenewRetry = time.Hour

func (w *Worker) renewDevice(ctx context.Context) error {
	token, err := identity.ReadTokenFile(filepath.Join(w.Root, "device.token"))
	if err != nil {
		return err
	}
	_, err = devicepair.Renew(ctx, nil, w.Config.Hub, token)
	return err
}
func (w *Worker) keepDeviceCredential(ctx context.Context) {
	renewalLoop(ctx, w.renewDevice, func() bool { w.mu.Lock(); defer w.mu.Unlock(); return w.ready }, func(ctx context.Context, d time.Duration) bool {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		}
	}, func() {
		log.Printf("WARN worker: device credential renewal failed; will retry (active work continues)")
	})
}

// Run independently of the command/socket path: one attempt at startup, then
// daily while connected. Failures and disconnected checks retry hourly. All
// calls are bounded by Renew's timeout and stop with the worker context.
func renewalLoop(ctx context.Context, renew func(context.Context) error, connected func() bool, wait func(context.Context, time.Duration) bool, failed func()) {
	first := true
	for ctx.Err() == nil {
		delay := deviceRenewRetry
		if first || connected() {
			first = false
			if err := renew(ctx); err == nil {
				delay = deviceRenewInterval
			} else if ctx.Err() == nil {
				failed()
			}
		}
		if !wait(ctx, delay) {
			return
		}
	}
}
