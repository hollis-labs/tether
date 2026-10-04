//go:build !windows

package app

import (
	"context"
	"encoding/json"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/store"
)

func (s *Service) shimHealth(id string) *api.ShimHealthStatus {
	if s.Store == nil {
		return nil
	}
	row, err := s.Store.SessionShim(context.Background(), id)
	if err != nil {
		return nil
	}
	status := &api.ShimHealthStatus{PlacementKey: row.ShimKey, Backend: row.HostBackend, Unit: row.UnitName, Socket: row.SocketPath}
	if session, err := s.Store.GetSession(id); err == nil {
		status.State = session.State
	}
	if events, err := s.Store.QueryEvents(store.EventFilter{SessionID: id, Kinds: []string{"session.shim_status"}, Limit: 1}); err == nil && len(events) == 1 {
		var last shimStatus
		if json.Unmarshal([]byte(events[0].PayloadJSON), &last) == nil {
			status.Reason = last.Reason
		}
	}
	if _, err := loadShimReceipt(row); err != nil {
		status.Reason = shimFailureCode(err)
	}
	return status
}
