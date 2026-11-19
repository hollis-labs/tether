package teamsvc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/libs/mesh/core/teams"
)

// RunDeadlines is the host's durable record of when each open run must stop and
// whether it has been asked to end early. Optional: without it the service
// dissolves inline on a final result and never reaps by timeout.
type RunDeadlines interface {
	RecordRunDeadline(ctx context.Context, runID string, deadline time.Time) error
	// GetRunDeadline reports false when the run has no open row.
	GetRunDeadline(ctx context.Context, runID string) (time.Time, bool, error)
	// RequestRunEnd reports false when the run has no open row.
	RequestRunEnd(ctx context.Context, runID, reason string) (bool, error)
	OpenRuns(ctx context.Context, limit int) ([]RunDeadline, error)
	CloseRun(ctx context.Context, runID string) error
}

// LifecyclePolicy is an optional Ceilings extension. DissolvesOnResult reports
// whether a result reported by a member in reporterSlot is the team's final
// result: the run then dissolves and every member session stops.
type LifecyclePolicy interface {
	DissolvesOnResult(t teams.Team, reporterSlot string) bool
	// ManagesLifecycle reports whether the daemon reaps runs of this team
	// (deadline, all-stopped). Teams it does not manage behave as before.
	ManagesLifecycle(t teams.Team) bool
}

// MemberAddPolicy is an optional Ceilings extension. For a lifecycle-managed
// team it decides whether one more member may be added to a run with this
// roster (shape and size caps); a nil error admits it.
type MemberAddPolicy interface {
	AdmitMemberAdd(t teams.Team, roster teams.Roster, slot string) error
}

const runDissolved mesh.TaskState = "dissolved"

// Reasons recorded for a run that ended without a caller dissolving it.
const (
	EndResult     = "final result reported"
	EndTimeout    = "timeout ceiling reached"
	EndAllStopped = "all members stopped"
)

func runEnded(s mesh.TaskState) bool { return s.Terminal() || s == runDissolved }

// endRunAsOwner dissolves a run on behalf of its owner (the governance member
// that formed it). It is the system path behind the final-result, timeout and
// all-stopped stops: no caller principal is involved, the retained owner seat
// supplies the administrative authority the library checks. The plan is the
// whole current roster, so every member session is stopped or released.
func (s *Service) endRunAsOwner(ctx context.Context, runID string) error {
	run, err := s.deps.Runs.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	t, err := s.deps.Definitions.GetDefinition(ctx, run.TeamID, run.TeamVersion)
	if err != nil {
		return err
	}
	if t.Authority.Mode != teams.Strict {
		return ErrDenied
	}
	roster, err := s.deps.Roster.Snapshot(ctx, runID)
	if err != nil {
		return err
	}
	var owner teams.Member
	for _, m := range roster.Members {
		if m.Governance == teams.Owner && m.Status == "active" {
			owner = m
			break
		}
	}
	if owner.ID == "" {
		return fmt.Errorf("%w: run has no active owner", ErrNotFound)
	}
	plan := terminationPlan{Actor: owner, Members: roster.Members}
	err = teams.EndRun(ctx, runID, guardedRoster{RosterStore: s.deps.Roster, team: t, plan: plan, provisioner: s.deps.Provisioner}, s.deps.Provisioner, s.deps.Routing)
	if err == nil {
		err = s.deps.Runs.MarkDissolved(ctx, runID)
	}
	if err == nil && s.deps.Deadlines != nil {
		err = s.deps.Deadlines.CloseRun(ctx, runID)
	}
	return err
}

// abortFormation tears down a run whose formation failed after members were
// launched: every member session is stopped and the run closed, so nothing is
// left running without an owner or a reaper row. Best effort; errors are joined.
func (s *Service) abortFormation(ctx context.Context, runID string) error {
	err := teams.EndRun(ctx, runID, s.deps.Roster, s.deps.Provisioner, s.deps.Routing)
	if err == nil {
		err = s.deps.Runs.MarkDissolved(ctx, runID)
	}
	return err
}

// ReapRuns is the daemon-driven lifecycle pass. For each open run it dissolves
// the run when it has been asked to end (final result), when its timeout
// ceiling has passed, or when none of its members is live any more. It is
// idempotent and safe to run concurrently with the team verbs; a run that
// fails to end is retried on the next pass.
func (s *Service) ReapRuns(ctx context.Context, limit int) error {
	if s.deps.Deadlines == nil {
		return nil
	}
	open, err := s.deps.Deadlines.OpenRuns(ctx, limit)
	if err != nil {
		return err
	}
	now := s.deps.Clock.Now()
	var failures []error
	for _, d := range open {
		if ctx.Err() != nil {
			break
		}
		run, err := s.deps.Runs.GetRun(ctx, d.RunID)
		switch {
		case errors.Is(err, ErrNotFound):
			failures = append(failures, s.deps.Deadlines.CloseRun(ctx, d.RunID))
			continue
		case err != nil:
			failures = append(failures, err)
			continue
		case runEnded(run.Status):
			failures = append(failures, s.deps.Deadlines.CloseRun(ctx, d.RunID))
			continue
		}
		reason := d.EndReason
		if reason == "" && !now.Before(d.Deadline) {
			reason = EndTimeout
		}
		if reason == "" {
			stopped, err := s.allStopped(ctx, d.RunID)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if stopped {
				reason = EndAllStopped
			}
		}
		if reason == "" {
			continue
		}
		if err = s.endRunAsOwner(ctx, d.RunID); err != nil {
			failures = append(failures, fmt.Errorf("end run %s (%s): %w", d.RunID, reason, err))
		}
	}
	return errors.Join(failures...)
}

// allStopped reports a run that has members and no live one among the
// non-owner members. The owner holds a governance seat with no session, so it
// is never what keeps a run alive.
func (s *Service) allStopped(ctx context.Context, runID string) (bool, error) {
	roster, err := s.deps.Roster.Snapshot(ctx, runID)
	if err != nil {
		return false, err
	}
	members := 0
	for _, m := range roster.Members {
		if m.Governance == teams.Owner {
			continue
		}
		members++
		if m.Status == "active" || m.Status == "provisioning" {
			return false, nil
		}
	}
	return members > 0, nil
}

// finishOnResult runs after a result was accepted. When the reporter's slot is
// the team's final reporter, the run is asked to end: the reaper dissolves it
// on its next pass (so the reporter's own call can return first). Without a
// deadline store the run is dissolved inline.
func (s *Service) finishOnResult(ctx context.Context, t teams.Team, runID string, reporter teams.Member) error {
	policy, ok := s.deps.Ceilings.(LifecyclePolicy)
	if !ok || !policy.DissolvesOnResult(t, reporter.Slot) {
		return nil
	}
	if s.deps.Deadlines != nil {
		found, err := s.deps.Deadlines.RequestRunEnd(ctx, runID, EndResult)
		if err != nil || found {
			return err
		}
		// No open reaper row (never recorded, or closed): a final result
		// must still dissolve the run, so end it directly.
	}
	return s.endRunAsOwner(ctx, runID)
}

// managesLifecycle reports whether a formed run of t gets a reaper deadline.
func (s *Service) managesLifecycle(t teams.Team) bool {
	policy, ok := s.deps.Ceilings.(LifecyclePolicy)
	return ok && s.deps.Deadlines != nil && policy.ManagesLifecycle(t)
}

// addMemberLimits returns the team, request limits and defaults a member add
// spawns under. Unmanaged (Stage A) runs are unchanged. A managed run adds
// under the ceilings recorded on its owner seat at formation and the run's
// deadline: the member's timeout is what remains of the run's, never more. The
// owner is the only seat that may spawn, and only into the slots the policy
// admits; the spawn grant is synthesized here for that one call and is not part
// of the stored team, so no member holds it.
func (s *Service) addMemberLimits(ctx context.Context, t teams.Team, actor teams.Member, req AddMemberRequest) (teams.Team, mesh.Limits, mesh.Limits, error) {
	if !s.managesLifecycle(t) {
		return t, req.Limits, s.deps.Defaults, nil
	}
	fail := func(err error) (teams.Team, mesh.Limits, mesh.Limits, error) {
		return t, mesh.Limits{}, mesh.Limits{}, err
	}
	if actor.Governance != teams.Owner {
		return fail(ErrDenied)
	}
	roster, err := s.deps.Roster.Snapshot(ctx, req.RunID)
	if err != nil {
		return fail(err)
	}
	if admit, ok := s.deps.Ceilings.(MemberAddPolicy); ok {
		if err = admit.AdmitMemberAdd(t, roster, req.Slot); err != nil {
			return fail(err)
		}
	}
	if err = actor.Limits.Validate(); err != nil {
		return fail(fmt.Errorf("%w: run has no recorded ceilings", ErrUnavailable))
	}
	limits := req.Limits
	at, found, err := s.deps.Deadlines.GetRunDeadline(ctx, req.RunID)
	if err != nil {
		return fail(err)
	}
	if found {
		remaining := at.Sub(s.deps.Clock.Now())
		if remaining <= 0 {
			return fail(fmt.Errorf("%w: run is past its deadline", ErrUnavailable))
		}
		if limits.Timeout == 0 || limits.Timeout > remaining {
			limits.Timeout = remaining
		}
	}
	grants := append([]teams.Grant(nil), t.Authority.Grants...)
	t.Authority.Grants = append(grants, teams.Grant{FromActor: actor.Actor, Verb: teams.MaySpawn, ToSlot: req.Slot})
	return t, limits, actor.Limits, nil
}
