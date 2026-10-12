package app

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launchprofile"
)

// BootResumeOptions explicitly protects selected direct canonical sources from
// ordinary boot recovery. Other agents retain their existing recovery behavior.
type BootResumeOptions struct {
	NativeOnlySourceIDs []string
	NativeOnlyWorkRoots map[string]string
}

// BootResumeSessions runs once after the listener is serving. Startup
// reconciliation has already reattached surviving shims. Ordinary agents need
// positive durable work evidence; selected native-only sources never receive an
// automatic recovery turn. Selection consumes no mailbox. An agent is resumed
// only when its catalog definition says recovery: auto (default never), and
// nothing is recovered while the workspace root is unusable.
func (s *Service) BootResumeSessions(ctx context.Context, options ...BootResumeOptions) {
	// Resolve every explicit source before any startup recovery. A missing or
	// enrolled source cannot fall through into ordinary selection.
	for _, option := range options {
		for id := range option.NativeOnlyWorkRoots {
			selected := false
			for _, sourceID := range option.NativeOnlySourceIDs {
				if sourceID == id {
					selected = true
				}
			}
			if !selected {
				log.Print("boot recovery: workroot requires selected native-only source")
				return
			}
		}
		for _, id := range option.NativeOnlySourceIDs {
			source, err := s.Store.GetSession(id)
			if err != nil || source.LogicalAgentID == "" {
				log.Print("boot recovery: native-only source unavailable")
				return
			}
			plan, err := s.Store.GetLaunchPlan(id)
			if err != nil || plan.TeamMember {
				log.Print("boot recovery: native-only direct source unavailable")
				return
			}
		}
	}
	if reason := s.bootReadinessFailure(); reason != "" {
		log.Printf("boot recovery: skipped all agents: %s", reason)
		return
	}
	if s.BootTeamRecovery != nil {
		if err := s.BootTeamRecovery(ctx); err != nil {
			log.Printf("boot recovery: retained members: %v", err)
		}
	}
	agents, err := s.Store.ListLogicalAgents()
	if err != nil {
		log.Printf("boot recovery: list agents: %v", err)
		return
	}
	for _, a := range agents {
		if ctx.Err() != nil {
			return
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		s.bootResumeAgent(attemptCtx, a.ID, options...)
		cancel()
	}
}

// bootReadinessFailure returns why boot recovery must not run at all, or "".
func (s *Service) bootReadinessFailure() string {
	if s.Catalog == nil {
		return "catalog unavailable"
	}
	root := config.ResolveWorkspaceRoot(s.Catalog.Global.Catalog.Defaults, s.Catalog.Paths)
	info, err := os.Stat(root)
	if err != nil {
		return "workspace_root " + root + " unavailable: " + err.Error()
	}
	if !info.IsDir() {
		return "workspace_root " + root + " is not a directory"
	}
	return ""
}

func (s *Service) bootResumeAgent(ctx context.Context, agentID string, options ...BootResumeOptions) {
	live, err := s.Store.AgentHasLiveSession(ctx, agentID)
	if err != nil || live {
		return
	}
	parent, err := s.Store.LatestSessionForAgent(ctx, agentID)
	if err != nil || parent == nil {
		return
	}
	if parent.WorkerID != "" {
		return
	}
	for _, option := range options {
		for _, id := range option.NativeOnlySourceIDs {
			source, err := s.Store.GetSession(id)
			if err != nil {
				s.bootResumeOutcome(agentID, parent.ID, "failed", "native_only_source_unavailable")
				return
			}
			if source.LogicalAgentID != agentID {
				continue
			}
			if parent.ID != id {
				s.bootResumeOutcome(agentID, parent.ID, "failed", "native_only_source_changed")
				return
			}
			result, err := s.ResumeLogicalAgentWithContext(ctx, agentID, api.ResumeOptions{NativeOnly: true, SourceSessionID: id, ResumeWorkRoot: option.NativeOnlyWorkRoots[id]})
			if err != nil {
				s.bootResumeOutcome(agentID, id, "failed", "native_only_resume_refused")
				return
			}
			s.bootResumeOutcome(agentID, result.SessionID, "resumed", "native_only")
			return
		}
	}
	plan, err := s.Store.GetLaunchPlan(parent.ID)
	if err != nil {
		s.bootResumeOutcome(agentID, parent.ID, "failed", "source_plan_unavailable")
		return
	}
	if plan.TeamMember {
		return
	} // separately recovered under the host's receipt
	// Explicit native-only selection above is an operator choice; everything
	// else needs the effective agent policy (extends cascade) to be auto.
	if reason, detail := s.bootRecoveryPolicyReason(ctx, agentID); reason != "" {
		if detail != "" {
			log.Printf("boot recovery: agent %s decision=skipped reason=%s: %s", agentID, reason, detail)
		} else {
			log.Printf("boot recovery: agent %s decision=skipped reason=%s", agentID, reason)
		}
		s.bootResumeOutcome(agentID, parent.ID, "skipped", reason)
		return
	}
	plan.ResumeSourceSessionID = parent.ID
	ck, err := s.Store.GetLatestCheckpointForAgent(agentID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.bootResumeOutcome(agentID, parent.ID, "failed", "checkpoint_unavailable")
		return
	}
	pack := s.recoveryContext(ctx, plan, ck)
	if pack.Unread == 0 && !pack.openAssignment && len(pack.Rosters) == 0 {
		return
	}
	result, err := s.ResumeLogicalAgentWithContext(ctx, agentID, api.ResumeOptions{})
	if err != nil {
		s.bootResumeOutcome(agentID, parent.ID, "failed", "resume_refused")
		log.Printf("boot recovery: agent %s: %v", agentID, err)
		return
	}
	s.bootResumeOutcome(agentID, result.SessionID, "resumed", "")
}

// agentOnlySource looks profiles up in Catalog.Agents only. Catalog.GetProfile
// prefers Launches[id], which would let a launch id shadow a logical agent (or
// an ancestor in its extends chain) when reading the recovery policy.
type agentOnlySource struct{ cat *config.Catalog }

func (a agentOnlySource) GetProfile(_ context.Context, id string) (*launchprofile.LaunchProfile, error) {
	if a.cat != nil {
		if p, ok := a.cat.Agents[id]; ok {
			cp := p
			return &cp, nil
		}
	}
	return nil, launchprofile.ErrProfileNotFound
}

func (agentOnlySource) GetContext(context.Context, string) (*launchprofile.LaunchContext, error) {
	return nil, launchprofile.ErrContextNotFound
}

// bootRecoveryPolicyReason returns "" when the agent's effective recovery
// policy (extends chain resolved over agents only) is auto, else the skip
// reason and, for an unresolved chain, the resolver error.
func (s *Service) bootRecoveryPolicyReason(ctx context.Context, agentID string) (reason, detail string) {
	if s.Catalog == nil {
		return "recovery_not_auto", ""
	}
	profile, err := launchprofile.NewResolver(agentOnlySource{s.Catalog}).ResolveProfile(ctx, agentID)
	if err != nil {
		return "recovery_policy_unresolved", err.Error()
	}
	if profile.Recovery != launchprofile.RecoveryAuto {
		return "recovery_not_auto", ""
	}
	return "", ""
}

func (s *Service) bootResumeOutcome(agentID, sessionID, outcome, reason string) {
	publishSessionEvent(s.Bus, sessionID, agentID, "session.boot_recovery", struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason,omitempty"`
	}{outcome, reason})
}
