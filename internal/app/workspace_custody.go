package app

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/tether/internal/store"
)

// escrowLaunchScratch has one release owner per allocation. A retry cannot
// overwrite held handles, even when the durable allocation identity matches.
func (s *Service) escrowLaunchScratch(custody *launchScratchCustody) error {
	key := launchScratchKey{custody.allocation.SessionID, custody.allocation.OperationID}
	prior, loaded := s.scratchCustodies.LoadOrStore(key, custody)
	if loaded && prior != custody {
		_ = custody.Close() // New, pre-entry descriptors only; old custody stays.
		return store.ErrWorkspaceAllocationConflict
	}
	return nil
}

func (s *Service) holdsLaunchScratch(id string) bool {
	held := false
	s.scratchCustodies.Range(func(key, _ any) bool {
		held = key.(launchScratchKey).session == id
		return !held
	})
	return held
}

func (s *Service) releaseLaunchScratch(custody *launchScratchCustody) {
	key := launchScratchKey{custody.allocation.SessionID, custody.allocation.OperationID}
	if s.scratchCustodies.CompareAndDelete(key, custody) {
		_ = custody.Close()
	}
}

func (s *Service) releaseScratchBeforeEntry(custody *launchScratchCustody, runtime *launchScratchRuntime) {
	if custody == nil || runtime != nil && runtime.entered.Load() {
		return
	}
	// Placement may precede bridge Start. Unknown placement/read failures are
	// retained; only the existing canonical unplaced path releases handles.
	if _, err := s.Store.SessionShim(context.Background(), custody.allocation.SessionID); errors.Is(err, store.ErrSessionShimNotFound) {
		s.releaseLaunchScratch(custody)
	}
}

// releaseScratchAfterCompletion is called only after successful Manager.Start.
// Observation uses the uncanceled launch context. Lookup or
// cancellation errors retain custody; a terminal ExitError is still completion.
// This uses Manager's result and never calls Session.Wait a second time.
func (s *Service) releaseScratchAfterCompletion(ctx context.Context, custody *launchScratchCustody) {
	_, err := s.Manager.WaitSession(ctx, custody.allocation.SessionID)
	if errors.Is(err, agentsessions.ErrSessionNotRunning) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	// A hosted bridge's completion is not the provider child's completion.
	// Retained shim custody (or a lookup error) remains escrowed for an exact
	// future reconciliation seam; this helper issues no child-death proof.
	if _, err := s.Store.SessionShim(ctx, custody.allocation.SessionID); !errors.Is(err, store.ErrSessionShimNotFound) {
		return
	}
	s.releaseLaunchScratch(custody)
}

// launchScratchRuntime preserves Runtime's complete interface and returns the
// ORIGINAL Session, including optional RPC/provider facets. entry means the
// underlying Start invocation actually occurred, not that a process is alive.
type launchScratchRuntime struct {
	agentsessions.Runtime
	custody *launchScratchCustody
	entered atomic.Bool
}

func (r *launchScratchRuntime) Start(ctx context.Context, opts agentsessions.StartOptions) (agentsessions.Session, error) {
	if err := r.custody.validateStart(ctx); err != nil {
		return nil, err
	}
	r.custody.mu.Lock()
	err := r.custody.validatePhysical(ctx)
	r.custody.mu.Unlock()
	if err != nil {
		return nil, err
	}
	r.entered.Store(true)
	return r.Runtime.Start(ctx, opts)
}

// Failed entered Starts remain escrowed: a generic Runtime error proves no
// process death. The private map plus durable receipt permit exact inspection;
// no automatic reconciliation/deletion is implemented. Process shutdown loses
// handles, while the receipt/directories remain and unknown partials refuse.
