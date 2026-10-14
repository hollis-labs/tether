//go:build !windows

package app

import (
	"context"
	"fmt"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/events"
)

// RetireShimSession is the explicit operator retirement of a detached shim
// session whose outcome is unknown (doctor "shim-detached"). It routes through
// the daemon, takes the session launch gate, and refuses unless the host
// positively reports the placement gone. Codex custody holds protocol inbox
// obligations that a gone host does not discharge, so those sessions also need
// acknowledgeObligations. Team members keep their team recovery path.
func (s *Service) RetireShimSession(ctx context.Context, id, actor string, acknowledgeObligations bool) error {
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", api.ErrShimRetireRefused, fmt.Sprintf(format, args...))
	}
	release, err := s.lockSessionLaunch(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	session, err := s.Store.GetSession(id)
	if err != nil {
		return err
	}
	if session.State != "detached" {
		return refuse("session is %q, not detached", session.State)
	}
	row, err := s.Store.SessionShim(ctx, id)
	if err != nil {
		return refuse("session has no recorded shim placement")
	}
	plan, err := s.Store.GetLaunchPlan(id)
	if err != nil {
		return refuse("launch plan unavailable: %v", err)
	}
	if plan.TeamMember {
		return refuse("team member sessions retire through team recovery")
	}
	if plan.ProviderBrand == "codex" && !acknowledgeObligations {
		return refuse("codex custody may hold undelivered obligations; pass acknowledge_obligations to retire")
	}
	receipt, err := loadShimReceipt(row)
	if err != nil {
		return refuse("cannot verify the host is gone: %v", err)
	}
	host, err := s.shimHost()
	if err != nil {
		return err
	}
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	inspection, err := host.inspect(vctx, receipt)
	if err != nil {
		return refuse("cannot verify the host is gone: %v", err)
	}
	if !inspection.Gone {
		return refuse("host or child is still present; stop the session instead")
	}
	if err := host.stopProvider(vctx, receipt); err != nil {
		return refuse("provider retirement failed: %v", err)
	}
	if err := s.MarkSessionOrphaned(id, "shim_retired_by_operator"); err != nil {
		return err
	}
	s.shimDiagnostic(id, &receipt, "orphaned", "operator_retired")
	publishSessionEvent(s.Bus, id, session.LogicalAgentID, events.KindSessionShimRetired, map[string]any{
		"actor": actor, "session_id": id, "placement_key": row.ShimKey, "backend": row.HostBackend,
		"brand": plan.ProviderBrand, "acknowledged_obligations": acknowledgeObligations,
	})
	return nil
}
