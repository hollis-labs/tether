package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hollis-labs/tether/internal/launch"
)

// HubResumeLineage describes an ordinary hub resume's stored UI ancestry. It is
// not native readiness, an acknowledgement or authority to execute anything.
type HubResumeLineage struct {
	Kind                 string `json:"kind"`
	SourceSessionID      string `json:"source_session_id"`
	ReplacementSessionID string `json:"replacement_session_id"`
	LogicalAgentID       string `json:"logical_agent_id"`
}

// GetHubResumeLineage covers ordinary orphan recovery, whose positively lost
// source remains orphaned. Team and worker recovery retain their distinct proof
// paths. Historical destinations need not still be running; the consumer checks
// liveness of its current tip separately.
func (s *Store) GetHubResumeLineage(ctx context.Context, id string) (*HubResumeLineage, error) {
	var r HubResumeLineage
	var sourceAgent, nextAgent, sourceWorker, nextWorker, sourceState, nextState, intent, sourceJSON, nextJSON string
	var custody bool
	err := s.db.QueryRowContext(ctx, `SELECT a.id,b.id,COALESCE(a.logical_agent_id,''),COALESCE(b.logical_agent_id,''),COALESCE(a.worker_id,''),COALESCE(b.worker_id,''),a.state,b.state,b.intent,COALESCE(p.plan_json,''),COALESCE(q.plan_json,''),EXISTS(SELECT 1 FROM session_shims WHERE session_id=a.id)
 FROM sessions b JOIN sessions a ON a.id=b.parent_session_id LEFT JOIN launch_plans p ON p.session_id=a.id LEFT JOIN launch_plans q ON q.session_id=b.id WHERE b.id=?`, id).Scan(&r.SourceSessionID, &r.ReplacementSessionID, &sourceAgent, &nextAgent, &sourceWorker, &nextWorker, &sourceState, &nextState, &intent, &sourceJSON, &nextJSON, &custody)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if intent != "resume" || sourceWorker != "" || nextWorker != "" || custody {
		return nil, nil
	}
	var sourcePlan, nextPlan launch.Plan
	if json.Unmarshal([]byte(sourceJSON), &sourcePlan) != nil || json.Unmarshal([]byte(nextJSON), &nextPlan) != nil {
		return nil, fmt.Errorf("hub resume plan unavailable")
	}
	if sourcePlan.TeamMember || nextPlan.TeamMember || nextPlan.ResumeSourceSessionID == "" {
		return nil, nil
	}
	if r.SourceSessionID == "" || r.SourceSessionID == id || r.ReplacementSessionID != id || sourceAgent == "" || sourceAgent != nextAgent || sourcePlan.LogicalAgentID != sourceAgent || nextPlan.LogicalAgentID != sourceAgent || nextPlan.ResumeSourceSessionID != r.SourceSessionID {
		return nil, fmt.Errorf("hub resume lineage identity mismatch")
	}
	if !lineageSettled(sourceState, true) || nextState != "running" && !lineageSettled(nextState, true) {
		return nil, nil
	}
	r.Kind, r.LogicalAgentID = "hub_resume_v1", sourceAgent
	return &r, nil
}
