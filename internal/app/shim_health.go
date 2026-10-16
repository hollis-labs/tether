//go:build !windows

package app

import (
	"context"
	"encoding/json"
	"github.com/hollis-labs/tether/internal/events"
	"time"

	"github.com/hollis-labs/tether/internal/api"
)

// shimHealthTTL bounds how stale a cached answer may be. The reply dispatcher and wake sweep ask for
// health of the same few sessions repeatedly; recomputing it each time was a measurable share of hub CPU.
const shimHealthTTL = 2 * time.Second

type cachedShimHealth struct {
	at     time.Time
	status *api.ShimHealthStatus
}

func (s *Service) shimHealth(id string) *api.ShimHealthStatus {
	if s.Store == nil {
		return nil
	}
	if v, ok := s.shimHealthCache.Load(id); ok {
		if c := v.(cachedShimHealth); time.Since(c.at) < shimHealthTTL {
			if c.status == nil {
				return nil
			}
			cp := *c.status
			return &cp
		}
	}
	status := s.computeShimHealth(id)
	s.shimHealthCache.Store(id, cachedShimHealth{at: time.Now(), status: status})
	if status == nil {
		return nil
	}
	cp := *status
	return &cp
}

func (s *Service) computeShimHealth(id string) *api.ShimHealthStatus {
	row, err := s.Store.SessionShim(context.Background(), id)
	if err != nil {
		return nil
	}
	status := &api.ShimHealthStatus{PlacementKey: row.ShimKey, Backend: row.HostBackend, Unit: row.UnitName, Socket: row.SocketPath}
	if session, err := s.Store.GetSession(id); err == nil {
		status.State = session.State
	}
	// Only the newest shim_status matters, and it is one indexed lookup (idx_events_session_kind_at);
	// QueryEvents would filter the whole session history for it.
	if last, err := s.Store.LatestSessionEvent(context.Background(), id, events.KindSessionShimStatus); err == nil && last != nil {
		var body shimStatus
		if json.Unmarshal([]byte(last.PayloadJSON), &body) == nil {
			status.Reason = body.Reason
		}
	}
	if _, err := loadShimReceipt(row); err != nil {
		status.Reason = shimFailureCode(err)
	}
	return status
}
