package app

import (
	"context"
	"errors"
	"github.com/hollis-labs/tether/internal/agencyregistry"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/store"
)

// RegisterExternalCorrelation queues kit-seat facts without creating a local
// Tether session or minting a Registry id. Provider session is the stable run
// key when the external launcher has no distinct execution-run identifier.
func (s *Service) RegisterExternalCorrelation(_ context.Context, in api.CorrelationRegisterRequest) (api.CorrelationRegisterResponse, error) {
	if err := in.Validate(); err != nil {
		return api.CorrelationRegisterResponse{}, err
	}
	if row, err := s.Store.GetSession(in.Session); err != nil && !errors.Is(err, store.ErrSessionNotFound) {
		return api.CorrelationRegisterResponse{}, err
	} else if row != nil {
		return api.CorrelationRegisterResponse{Reason: "tether_session"}, nil
	}
	cat := s.cat()
	if cat == nil || cat.Global.AgencyRegistry.URL == "" {
		return api.CorrelationRegisterResponse{Reason: "disabled"}, nil
	}
	if in.RunID == "" {
		in.RunID = in.Session
	}
	facts := agencyregistry.Facts{URN: in.URN, Session: in.Session, RunID: in.RunID, TaskID: in.TaskID, WorkstreamID: in.WorkstreamID, ManifestDigest: in.ManifestDigest, ExportReceiptID: in.ExportReceiptID, ActivitySHA256: in.ActivitySHA256, ProviderSessionID: in.ProviderSessionID}
	if !s.queueCorrelation(cat.Global.AgencyRegistry, facts, "tether-hook:", nil) {
		return api.CorrelationRegisterResponse{Reason: "queue_unavailable"}, nil
	}
	return api.CorrelationRegisterResponse{Queued: true}, nil
}
