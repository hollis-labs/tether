//go:build !windows

package app

import (
	"context"
	"path/filepath"
	"time"

	"github.com/hollis-labs/tether/internal/shimbridge"
	"github.com/hollis-labs/tether/internal/shimhost"
)

const shimHandshakeTimeout = 10 * time.Second

func (h *shimHosting) waitHandshake(ctx context.Context, r shimhost.Receipt, previous string) error {
	if h.handshake != nil {
		return h.handshake(ctx, r, previous)
	}
	return waitShimHandshake(ctx, r, previous, shimbridge.ReadCheckpoint)
}

// Only a fresh, matching checkpoint written after hello establishes readiness.
// An old controller's checkpoint must never authorize a starting bridge.
func waitShimHandshake(ctx context.Context, r shimhost.Receipt, previous string, read func(string) (shimbridge.Checkpoint, error)) error {
	ctx, cancel := context.WithTimeout(ctx, shimHandshakeTimeout)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return &shimhost.Failure{Code: "handshake_pending", Message: "bridge controller handshake not durably settled before deadline"}
		}
		state, err := read(filepath.Join(filepath.Dir(r.DescriptorPath), "bridge.json"))
		if err != nil {
			return &shimhost.Failure{Code: "outcome_unknown", Message: "bridge checkpoint unavailable during handshake"}
		}
		if state.Session != r.Session || state.Instance != r.Instance || state.Generation != r.Generation || state.Journal != r.Journal {
			return &shimhost.Failure{Code: "identity_mismatch", Message: "bridge readiness checkpoint differs from placement"}
		}
		if state.ControllerEpoch != "" && state.ControllerEpoch != previous {
			return nil
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}
