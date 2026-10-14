package app

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/workerhub"
)

// LineageLimits caps plain (non-team) session lineage. A zero field means
// unlimited, so the zero value leaves every existing launch path unchanged.
type LineageLimits struct {
	// MaxDepth is the most ancestors a new session may have.
	MaxDepth int
	// MaxChildren is the most live (not ended) direct children one parent may have.
	MaxChildren int
	// MaxLaunchesPerWindow is the most children (live or ended) one parent may
	// launch within LaunchWindow; it bounds fan-out rate, not concurrency.
	MaxLaunchesPerWindow int
	// LaunchWindow defaults to a minute when MaxLaunchesPerWindow is set.
	LaunchWindow time.Duration
}

// lineageLaunchWindow parses the configured window; empty or invalid means the
// one-minute default.
func lineageLaunchWindow(s string) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return time.Minute
}

// ErrLineageLimit is returned when a new child would exceed LineageLimits.
var ErrLineageLimit error = lineageLimitError{}

// lineageLimitError carries a typed code so the API answers 409 (ADR 0010).
type lineageLimitError struct{}

func (lineageLimitError) Error() string     { return "session lineage limit exceeded" }
func (lineageLimitError) ErrorCode() string { return "lineage_limit_exceeded" }

// checkLineage refuses a child of parentID that would breach the configured
// limits. An empty parentID (a root session) is always allowed.
func (s *Service) checkLineage(parentID string) error {
	if parentID == "" {
		return nil
	}
	lim := s.LineageLimits
	if lim.MaxChildren > 0 {
		live, err := s.Store.LiveChildCount(parentID)
		if err != nil {
			return err
		}
		if live >= lim.MaxChildren {
			return fmt.Errorf("%w: parent %s already has %d live children (max %d)", ErrLineageLimit, parentID, live, lim.MaxChildren)
		}
	}
	if lim.MaxLaunchesPerWindow > 0 {
		w := lim.LaunchWindow
		if w <= 0 {
			w = time.Minute
		}
		n, err := s.Store.ChildCountSince(parentID, time.Now().Add(-w).UTC().Format(time.RFC3339))
		if err != nil {
			return err
		}
		if n >= lim.MaxLaunchesPerWindow {
			return fmt.Errorf("%w: parent %s already launched %d children in the last %s (max %d)", ErrLineageLimit, parentID, n, w, lim.MaxLaunchesPerWindow)
		}
	}
	if lim.MaxDepth > 0 {
		d, err := s.Store.SessionLineageDepth(parentID)
		if err != nil {
			return err
		}
		// The new child sits one level below the parent's own depth.
		if d+1 > lim.MaxDepth {
			return fmt.Errorf("%w: child of %s would be at depth %d (max %d)", ErrLineageLimit, parentID, d+1, lim.MaxDepth)
		}
	}
	return nil
}

// CheckLineage is the exported admission check for a new child of parentID,
// used by the session bootstrap route.
func (s *Service) CheckLineage(parentID string) error { return s.checkLineage(parentID) }

// AdmitChild runs the lineage check and create under one lock, so concurrent
// children of the same parent cannot both pass the check before either row
// exists. create must persist the child row before returning.
func (s *Service) AdmitChild(parentID string, create func() error) error {
	s.lineageMu.Lock()
	defer s.lineageMu.Unlock()
	if err := s.checkLineage(parentID); err != nil {
		return err
	}
	return create()
}

// StopSessionCascade stops id's descendants (deepest first) and then id. A
// descendant that is not running is skipped; the first real stop error is
// returned after every session has been attempted.
func (s *Service) StopSessionCascade(id string) error {
	var order []string
	seen := map[string]bool{id: true}
	var walk func(string) error
	walk = func(cur string) error {
		kids, err := s.Store.ChildSessionIDs(cur)
		if err != nil {
			return err
		}
		for _, k := range kids {
			if seen[k] {
				continue
			}
			seen[k] = true
			if err := walk(k); err != nil {
				return err
			}
			order = append(order, k)
		}
		return nil
	}
	if err := walk(id); err != nil {
		return err
	}
	order = append(order, id)
	var first error
	for _, sid := range order {
		err := s.stopCascadeMember(sid)
		if err != nil && !errors.Is(err, agentsessions.ErrSessionNotRunning) && first == nil {
			first = fmt.Errorf("stop %s: %w", sid, err)
		}
	}
	return first
}

// stopCascadeMember stops one session of a cascade. A worker-placed session is
// stopped through its worker; if that fails for a session the hub already
// records as ended, or the worker no longer knows it, there is nothing left to
// stop and the cascade is not failed by it. A live worker session whose worker
// is unreachable still reports the error.
func (s *Service) stopCascadeMember(id string) error {
	row, worker := s.workerSession(id)
	if !worker {
		return s.StopSession(id)
	}
	if s.Workers == nil {
		return fmt.Errorf("worker session %s: %w", id, workerhub.ErrChannelUnavailable)
	}
	err := s.StopSession(id)
	if err != nil && (session.State(row.State).Terminal() || strings.Contains(err.Error(), "unknown execution")) {
		return nil
	}
	return err
}
