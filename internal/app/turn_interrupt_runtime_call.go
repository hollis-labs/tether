package app

import (
	"context"
	"sync/atomic"
)

// The runtime can block before its context-aware ACK wait (for example on a
// full stdin pipe). Its late return only writes this private buffered channel;
// it never acquires/releases the submission gate or changes a turn marker.
func (s *Service) interruptRuntimeCall(ctx context.Context, sessionID string) error {
	returned := make(chan error, 1)
	var finished atomic.Bool
	defer finished.Store(true)
	go func() {
		err := s.Manager.InterruptTurn(ctx, sessionID)
		if !finished.Load() {
			returned <- err
		}
	}()
	select {
	case err := <-returned:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
