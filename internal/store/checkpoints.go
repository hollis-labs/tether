package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/chrispian/agent-mux/internal/checkpoint"
)

// CheckpointRow mirrors the checkpoints table. Nullable TEXT columns
// are exposed as Go strings (empty string ↔ SQL NULL) to keep callers
// out of sql.NullString for v0.0.2 ergonomics.
type CheckpointRow = checkpoint.Checkpoint

// CreateCheckpoint inserts a new checkpoint row. ID + LogicalAgentID +
// CreatedAt are required; all other fields are optional and persisted
// as SQL NULL when empty.
func (s *Store) CreateCheckpoint(c checkpoint.Checkpoint) error {
	if c.ID == "" {
		return errors.New("checkpoint id required")
	}
	if c.LogicalAgentID == "" {
		return errors.New("checkpoint logical_agent_id required")
	}
	if c.CreatedAt == "" {
		return errors.New("checkpoint created_at required")
	}
	_, err := s.db.Exec(
		`INSERT INTO checkpoints (
            id, logical_agent_id, task_id, workflow_id, status,
            completed_work, pending_work, key_decisions,
            referenced_artifacts, summary, next_recommendation,
            created_at, source_session_id, provider_hints
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.LogicalAgentID,
		nullIfEmpty(c.TaskID), nullIfEmpty(c.WorkflowID), nullIfEmpty(c.Status),
		nullIfEmpty(c.CompletedWork), nullIfEmpty(c.PendingWork),
		nullIfEmpty(c.KeyDecisions), nullIfEmpty(c.ReferencedArtifacts),
		nullIfEmpty(c.Summary), nullIfEmpty(c.NextRecommendation),
		c.CreatedAt, nullIfEmpty(c.SourceSessionID), nullIfEmpty(c.ProviderHintsJSON),
	)
	if err != nil {
		return fmt.Errorf("insert checkpoint %q: %w", c.ID, err)
	}
	return nil
}

// GetCheckpoint fetches a single checkpoint by id. Returns
// sql.ErrNoRows if no such row exists.
func (s *Store) GetCheckpoint(id string) (*checkpoint.Checkpoint, error) {
	var (
		c                                                                                                         checkpoint.Checkpoint
		taskID, workflowID, status, completed, pending, keyDec, artifacts, summary, nextRec, sourceSess, provHints sql.NullString
	)
	err := s.db.QueryRow(
		`SELECT id, logical_agent_id, task_id, workflow_id, status,
                completed_work, pending_work, key_decisions,
                referenced_artifacts, summary, next_recommendation,
                created_at, source_session_id, provider_hints
           FROM checkpoints WHERE id=?`,
		id,
	).Scan(&c.ID, &c.LogicalAgentID, &taskID, &workflowID, &status,
		&completed, &pending, &keyDec, &artifacts, &summary, &nextRec,
		&c.CreatedAt, &sourceSess, &provHints)
	if err != nil {
		return nil, err
	}
	c.TaskID = taskID.String
	c.WorkflowID = workflowID.String
	c.Status = status.String
	c.CompletedWork = completed.String
	c.PendingWork = pending.String
	c.KeyDecisions = keyDec.String
	c.ReferencedArtifacts = artifacts.String
	c.Summary = summary.String
	c.NextRecommendation = nextRec.String
	c.SourceSessionID = sourceSess.String
	c.ProviderHintsJSON = provHints.String
	return &c, nil
}

// GetLatestCheckpointForAgent returns the most recent checkpoint for the given
// logical agent (ordered by created_at DESC, LIMIT 1). Returns sql.ErrNoRows
// if no checkpoints exist for the agent.
func (s *Store) GetLatestCheckpointForAgent(logicalAgentID string) (*checkpoint.Checkpoint, error) {
	var (
		c                                                                                                         checkpoint.Checkpoint
		taskID, workflowID, status, completed, pending, keyDec, artifacts, summary, nextRec, sourceSess, provHints sql.NullString
	)
	err := s.db.QueryRow(
		`SELECT id, logical_agent_id, task_id, workflow_id, status,
                completed_work, pending_work, key_decisions,
                referenced_artifacts, summary, next_recommendation,
                created_at, source_session_id, provider_hints
           FROM checkpoints WHERE logical_agent_id=?
           ORDER BY created_at DESC LIMIT 1`,
		logicalAgentID,
	).Scan(&c.ID, &c.LogicalAgentID, &taskID, &workflowID, &status,
		&completed, &pending, &keyDec, &artifacts, &summary, &nextRec,
		&c.CreatedAt, &sourceSess, &provHints)
	if err != nil {
		return nil, err
	}
	c.TaskID = taskID.String
	c.WorkflowID = workflowID.String
	c.Status = status.String
	c.CompletedWork = completed.String
	c.PendingWork = pending.String
	c.KeyDecisions = keyDec.String
	c.ReferencedArtifacts = artifacts.String
	c.Summary = summary.String
	c.NextRecommendation = nextRec.String
	c.SourceSessionID = sourceSess.String
	c.ProviderHintsJSON = provHints.String
	return &c, nil
}

// ListCheckpointsByLogicalAgent returns all checkpoints for the given
// logical agent, newest first.
func (s *Store) ListCheckpointsByLogicalAgent(logicalAgentID string) ([]checkpoint.Checkpoint, error) {
	rows, err := s.db.Query(
		`SELECT id, logical_agent_id, task_id, workflow_id, status,
                completed_work, pending_work, key_decisions,
                referenced_artifacts, summary, next_recommendation,
                created_at, source_session_id, provider_hints
           FROM checkpoints WHERE logical_agent_id=? ORDER BY created_at DESC`,
		logicalAgentID,
	)
	if err != nil {
		return nil, fmt.Errorf("list checkpoints: %w", err)
	}
	defer rows.Close()
	var out []checkpoint.Checkpoint
	for rows.Next() {
		var (
			c                                                                                                         checkpoint.Checkpoint
			taskID, workflowID, status, completed, pending, keyDec, artifacts, summary, nextRec, sourceSess, provHints sql.NullString
		)
		if err := rows.Scan(&c.ID, &c.LogicalAgentID, &taskID, &workflowID, &status,
			&completed, &pending, &keyDec, &artifacts, &summary, &nextRec,
			&c.CreatedAt, &sourceSess, &provHints); err != nil {
			return nil, fmt.Errorf("scan checkpoint: %w", err)
		}
		c.TaskID = taskID.String
		c.WorkflowID = workflowID.String
		c.Status = status.String
		c.CompletedWork = completed.String
		c.PendingWork = pending.String
		c.KeyDecisions = keyDec.String
		c.ReferencedArtifacts = artifacts.String
		c.Summary = summary.String
		c.NextRecommendation = nextRec.String
		c.SourceSessionID = sourceSess.String
		c.ProviderHintsJSON = provHints.String
		out = append(out, c)
	}
	return out, rows.Err()
}
