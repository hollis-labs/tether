package app

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/hollis-labs/tether/internal/agent"
	"github.com/hollis-labs/tether/internal/checkpoint"
	"github.com/hollis-labs/tether/internal/store"
)

func (s *Service) GetLogicalAgentPolicy(logicalAgentID string) (agent.LogicalAgentPolicy, error) {
	return s.Store.GetLogicalAgentPolicy(logicalAgentID)
}

func (s *Service) UpdateLogicalAgentPolicy(policy agent.LogicalAgentPolicy) (agent.LogicalAgentPolicy, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	if err := s.Store.UpdateLogicalAgentPolicy(policy, now); err != nil {
		return agent.LogicalAgentPolicy{}, err
	}
	return s.Store.GetLogicalAgentPolicy(policy.LogicalAgentID)
}

func (s *Service) CreatePolicyCheckpoint(row *store.SessionRow, policy agent.LogicalAgentPolicy) error {
	if row == nil {
		return fmt.Errorf("session row required")
	}
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate checkpoint id: %w", err)
	}
	status := policy.CheckpointStatus
	if status == "" {
		status = "auto-stop"
	}
	c := checkpoint.Checkpoint{
		ID:             id.String(),
		LogicalAgentID: row.LogicalAgentID,
		Status:         status,
		Summary:        autoStopCheckpointSummary(*row),
		KeyDecisions:   "Checkpoint was created automatically because logical agent checkpoint_policy=on_stop.",
		NextRecommendation: fmt.Sprintf(
			"Resume logical agent %s to continue from this stop boundary.",
			row.LogicalAgentID,
		),
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
		SourceSessionID: row.ID,
	}
	if err := s.Store.CreateCheckpoint(c); err != nil {
		return err
	}
	return nil
}

func autoStopCheckpointSummary(row store.SessionRow) string {
	return fmt.Sprintf(
		"Auto-checkpoint created before stop for session %s (launch=%s provider=%s state=%s workspace=%s).",
		row.ID,
		emptyAs(row.LaunchID, "manual"),
		emptyAs(row.ProviderID, "unknown"),
		emptyAs(row.State, "unknown"),
		emptyAs(row.Workspace, "unknown"),
	)
}

func emptyAs(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
