package app

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	"github.com/hollis-labs/tether/internal/api"
)

// BootResumeSessions runs once after the listener is serving: daemon-owned MCP
// planting needs that listener. Startup reconciliation has already reattached
// surviving shims and identified disappeared children. Idle agents remain cold;
// unread mail, a recorded open assignment or active durable/pool membership is
// positive work evidence. No mailbox is consumed by this selection.
func (s *Service) BootResumeSessions(ctx context.Context) {
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
		s.bootResumeAgent(attemptCtx, a.ID)
		cancel()
	}
}

func (s *Service) bootResumeAgent(ctx context.Context, agentID string) {
	live, err := s.Store.AgentHasLiveSession(ctx, agentID)
	if err != nil || live {
		return
	}
	parent, err := s.Store.LatestSessionForAgent(ctx, agentID)
	if err != nil || parent == nil {
		return
	}
	plan, err := s.Store.GetLaunchPlan(parent.ID)
	if err != nil {
		s.bootResumeOutcome(agentID, parent.ID, "failed", "source_plan_unavailable")
		return
	}
	if plan.TeamMember {
		return
	} // separately recovered under the host's receipt
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

func (s *Service) bootResumeOutcome(agentID, sessionID, outcome, reason string) {
	publishSessionEvent(s.Bus, sessionID, agentID, "session.boot_recovery", struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason,omitempty"`
	}{outcome, reason})
}
