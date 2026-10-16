package daemon

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
)

type DeviceExpiryHealth struct {
	identity.DeviceExpiryCounts
	Unavailable bool `json:"unavailable,omitempty"`
}

// DenyListWatchHealth exposes aggregates only, never raw log lines or paths.
// The external file detector writes this status atomically along with its cursor.
type DenyListWatchHealth struct {
	GapCount             uint64     `json:"gap_count"`
	AcknowledgedGapCount uint64     `json:"acknowledged_gap_count"`
	Backlog              bool       `json:"backlog"`
	WarningCount         uint64     `json:"warning_count"`
	AcknowledgedCount    uint64     `json:"acknowledged_count"`
	CheckedAt            time.Time  `json:"checked_at"`
	LastFailureAt        *time.Time `json:"last_failure_at,omitempty"`
	Unavailable          bool       `json:"unavailable,omitempty"`
	Stale                bool       `json:"stale,omitempty"`
}

func (s *Server) deviceExpiryHealth() *DeviceExpiryHealth {
	if s.Identity == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	counts, err := s.Identity.DeviceExpiry(ctx, time.Now())
	return &DeviceExpiryHealth{DeviceExpiryCounts: counts, Unavailable: err != nil}
}

func denyListWatchHealth(path string, now time.Time) *DenyListWatchHealth {
	if path == "" {
		return nil
	}
	h := &DenyListWatchHealth{Unavailable: true}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: daemon operator-selected detector status path, not request input.
	if err != nil {
		return h
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return h
	}
	b, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(b) > 16384 || json.Unmarshal(b, h) != nil || h.CheckedAt.IsZero() || h.AcknowledgedCount > h.WarningCount || h.AcknowledgedGapCount > h.GapCount {
		return &DenyListWatchHealth{Unavailable: true}
	}
	h.Unavailable = false
	h.Stale = now.Sub(h.CheckedAt) > 5*time.Minute || h.CheckedAt.After(now.Add(time.Minute))
	return h
}
