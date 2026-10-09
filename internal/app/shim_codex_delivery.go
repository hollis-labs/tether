//go:build !windows

package app

import (
	"context"
	"strconv"

	"github.com/hollis-labs/go-agent-wrapper/turnoutput"
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

func (s *Service) deliverCodexInbox(ctx context.Context, observed shimcodex.State) (shimcodex.Projection, error) {
	if err := s.currentShimExecution(ctx, observed.Binding.Session); err != nil {
		return shimcodex.Projection{}, err
	}
	row, err := s.Store.SessionShim(ctx, observed.Binding.Session)
	if err != nil {
		return shimcodex.Projection{}, err
	}
	canonical, err := loadShimReceipt(row)
	if err != nil || canonical.Retired || codexBinding(canonical) != observed.Binding {
		return shimcodex.Projection{}, store.ErrSessionShimConflict
	}
	if _, err = s.Store.ReadCodexCandidate(ctx, observed); err != nil {
		return shimcodex.Projection{}, err
	}
	p, err := shimcodex.BuildDeliveryProjection(observed)
	if err != nil {
		return p, err
	}
	for i := range p.Turns {
		turn := &p.Turns[i]
		if turn.Phase == "open" || turn.OutputAcceptanceID != "" {
			continue
		}
		kind := turnoutput.KindFinal
		switch turn.Phase {
		case "failed":
			kind = turnoutput.KindFailure
		case "interrupted":
			kind = turnoutput.KindTerminal
		}
		result := turnoutput.Output{SessionID: observed.Binding.Session, TurnID: turn.StableOutputTurnID, Text: shimcodex.ProjectedTurnText(*turn), Kind: kind, StopReason: turn.StopReason, Runtime: "codex", Confidence: turnoutput.ConfidenceExact}
		messageID, outputID, err := s.persistHostedTurnOutput(ctx, observed.Binding.Session, result, turn.CompletionSourceID)
		if err != nil {
			return p, err
		}
		turn.OutputAcceptanceID = outputID
		if messageID != "" {
			turn.OutboxMessageIDs = []string{messageID}
		}
	}
	// Canonical filesystem custody is frozen across external publication. The
	// store transaction independently fences placement and the entire ledger.
	row, err = s.Store.SessionShim(ctx, observed.Binding.Session)
	if err != nil {
		return p, err
	}
	current, err := loadShimReceipt(row)
	if err != nil || current.Retired || current != canonical || s.stops.requested(observed.Binding.Session) {
		return p, store.ErrSessionShimConflict
	}
	return p, ctx.Err()
}
