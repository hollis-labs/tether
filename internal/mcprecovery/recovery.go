// Package mcprecovery bounds replacement handshakes after an MCP peer restarts.
// It must never wrap an operation whose execution outcome is ambiguous.
package mcprecovery

import (
	"context"
	"time"
)

const Budget = 60 * time.Second

// HandshakeTimeout bounds each initialization within the aggregate recovery budget.
const HandshakeTimeout = 15 * time.Second

// Run retries only the supplied handshake. Retryability belongs to the caller,
// which can distinguish admission errors from a temporarily absent server.
func Run(ctx context.Context, attempt func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, Budget)
	defer cancel()
	delay := time.Second
	for {
		retry, err := attempt(ctx)
		if err == nil || !retry {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, 30*time.Second)
	}
}
