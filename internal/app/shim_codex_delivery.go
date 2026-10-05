//go:build !windows

package app

import (
	"context"
	"strconv"

	"github.com/hollis-labs/tether/internal/shimcodex"
	"github.com/hollis-labs/tether/internal/store"
)

// codexDeliveryCandidate computes only a frozen pure candidate. It does not
// consume inbox events, stage output, invoke callbacks or certify delivery.
// It is not connected to the legacy reducer/publisher or a success issuer.
func (s *Service) codexDeliveryCandidate(ctx context.Context, observed shimcodex.State, previous shimcodex.Projection) (shimcodex.Projection, error) {
	snapshot, err := s.Store.ReadCodexCandidate(ctx, observed)
	if err != nil {
		return shimcodex.Projection{}, err
	}
	if previous.Binding != observed.Binding || previous.ProtocolRevision > observed.Revision {
		return shimcodex.Projection{}, store.ErrSessionShimConflict
	}
	// Freeze from the same transaction; no separately loaded mutable inbox.
	candidate := previous
	candidate.ProtocolRevision = snapshot.Protocol.Revision
	for _, event := range snapshot.Protocol.Inbox {
		candidate, err = shimcodex.ProjectFrozenSource(candidate, event)
		if err != nil {
			return shimcodex.Projection{}, err
		}
	}
	candidate.ReplayHighWater = snapshot.Protocol.ReplayHighWater
	candidate.StdoutOffset = snapshot.Protocol.StreamOffset
	candidate.PartialStart = snapshot.Protocol.PartialStart
	candidate.PartialBytes = append([]byte(nil), snapshot.Protocol.Partial...)
	candidate.OutstandingInputIDs = nil
	for _, op := range snapshot.Protocol.Operations {
		if op.EffectUnknown || op.Phase == shimcodex.Intent || op.Phase == shimcodex.Attempted || op.Phase == shimcodex.Written {
			candidate.OutstandingInputIDs = append(candidate.OutstandingInputIDs, strconv.FormatUint(op.ID, 10))
		}
	}
	candidate.OutstandingServerRequestSources = nil
	for _, request := range snapshot.Protocol.ServerRequests {
		if !request.Written {
			candidate.OutstandingServerRequestSources = append(candidate.OutstandingServerRequestSources, request.Source)
		}
	}
	if err := shimcodex.ValidateProjection(candidate); err != nil {
		return shimcodex.Projection{}, err
	}
	if err := ctx.Err(); err != nil {
		return shimcodex.Projection{}, err
	}
	// Successful B2 remains unearned; return a private candidate + Unsupported.
	return candidate, store.ErrCodexDeliveryUnsupported
}
