package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrCheckpointNotFound is returned for an unknown checkpoint, or an agent with none.
var ErrCheckpointNotFound = errors.New("checkpoint not found")

// CheckpointRow is one checkpoints row (checkpoint.Checkpoint in the old tree). Empty strings are
// stored as NULL for the optional columns. ProviderHintsJSON is opaque JSON, round-tripped as is.
type CheckpointRow struct {
	ID, LogicalAgentID, TaskID, WorkflowID, Status   string
	CompletedWork, PendingWork, KeyDecisions         string
	ReferencedArtifacts, Summary, NextRecommendation string
	CreatedAt, SourceSessionID, ProviderHintsJSON    string
}

const wsCheckpointColumns = `id, logical_agent_id, task_id, workflow_id, status, completed_work, pending_work, key_decisions,
 referenced_artifacts, summary, next_recommendation, created_at, source_session_id, provider_hints`

func wsScanCheckpoint(scan func(...any) error) (CheckpointRow, error) {
	var c CheckpointRow
	var task, workflow, status, completed, pending, decisions, artifacts, summary, next, source, hints sql.NullString
	if err := scan(&c.ID, &c.LogicalAgentID, &task, &workflow, &status, &completed, &pending, &decisions, &artifacts,
		&summary, &next, &c.CreatedAt, &source, &hints); err != nil {
		return CheckpointRow{}, err
	}
	c.TaskID, c.WorkflowID, c.Status = task.String, workflow.String, status.String
	c.CompletedWork, c.PendingWork, c.KeyDecisions = completed.String, pending.String, decisions.String
	c.ReferencedArtifacts, c.Summary, c.NextRecommendation = artifacts.String, summary.String, next.String
	c.SourceSessionID, c.ProviderHintsJSON = source.String, hints.String
	return c, nil
}

// CreateCheckpoint stores a checkpoint. ID, LogicalAgentID and CreatedAt are required.
func (s *Store) CreateCheckpoint(ctx context.Context, c CheckpointRow) error {
	switch {
	case c.ID == "":
		return errors.New("checkpoint id required")
	case c.LogicalAgentID == "":
		return errors.New("checkpoint logical_agent_id required")
	case c.CreatedAt == "":
		return errors.New("checkpoint created_at required")
	}
	if err := s.exec(ctx, `INSERT INTO checkpoints (`+wsCheckpointColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.LogicalAgentID, nullText(c.TaskID), nullText(c.WorkflowID), nullText(c.Status), nullText(c.CompletedWork),
		nullText(c.PendingWork), nullText(c.KeyDecisions), nullText(c.ReferencedArtifacts), nullText(c.Summary),
		nullText(c.NextRecommendation), c.CreatedAt, nullText(c.SourceSessionID), nullText(c.ProviderHintsJSON)); err != nil {
		return fmt.Errorf("insert checkpoint %q: %w", c.ID, err)
	}
	return nil
}

// GetCheckpoint returns one checkpoint (ErrCheckpointNotFound when unknown; the old store returned
// sql.ErrNoRows).
func (s *Store) GetCheckpoint(ctx context.Context, id string) (CheckpointRow, error) {
	return s.wsCheckpoint(ctx, `id = ?`, id)
}

// LatestCheckpointForAgent returns the agent's newest checkpoint (ErrCheckpointNotFound when it has
// none). Ties on created_at break by id, highest first (the old query left them unordered).
func (s *Store) LatestCheckpointForAgent(ctx context.Context, logicalAgentID string) (CheckpointRow, error) {
	return s.wsCheckpoint(ctx, `logical_agent_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`, logicalAgentID)
}

func (s *Store) wsCheckpoint(ctx context.Context, where string, arg any) (CheckpointRow, error) {
	r := s.c.Reader()
	c, err := wsScanCheckpoint(r.QueryRowContext(ctx, rebind(r, `SELECT `+wsCheckpointColumns+` FROM checkpoints WHERE `+where), arg).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return CheckpointRow{}, ErrCheckpointNotFound
	}
	return c, err
}

// ListCheckpointsByLogicalAgent returns the agent's checkpoints, newest first (ties by id, highest
// first).
func (s *Store) ListCheckpointsByLogicalAgent(ctx context.Context, logicalAgentID string) ([]CheckpointRow, error) {
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, rebind(r, `SELECT `+wsCheckpointColumns+` FROM checkpoints WHERE logical_agent_id = ? ORDER BY created_at DESC, id DESC`), logicalAgentID)
	if err != nil {
		return nil, fmt.Errorf("list checkpoints: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []CheckpointRow
	for rows.Next() {
		c, err := wsScanCheckpoint(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan checkpoint: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ClientAttachmentRow is one client_attachments row. Times are RFC 3339 text the caller formats;
// DetachedAt is "" while attached.
type ClientAttachmentRow struct {
	ID, SessionID, ClientKind, AttachedAt, DetachedAt string
}

// CreateClientAttachment stores a new attachment.
func (s *Store) CreateClientAttachment(ctx context.Context, id, sessionID, clientKind, attachedAt string) error {
	if err := s.exec(ctx, `INSERT INTO client_attachments (id, session_id, client_kind, attached_at) VALUES (?, ?, ?, ?)`,
		id, sessionID, clientKind, attachedAt); err != nil {
		return fmt.Errorf("insert client_attachment: %w", err)
	}
	return nil
}

// DetachClientAttachment stamps detached_at (a repeat rewrites it, as before).
func (s *Store) DetachClientAttachment(ctx context.Context, id, detachedAt string) error {
	if err := s.exec(ctx, `UPDATE client_attachments SET detached_at = ? WHERE id = ?`, detachedAt, id); err != nil {
		return fmt.Errorf("update client_attachment: %w", err)
	}
	return nil
}

// ListClientAttachments returns a session's attachments, oldest first (ties by id).
func (s *Store) ListClientAttachments(ctx context.Context, sessionID string) ([]ClientAttachmentRow, error) {
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, rebind(r, `SELECT id, session_id, client_kind, attached_at, detached_at
 FROM client_attachments WHERE session_id = ? ORDER BY attached_at, id`), sessionID)
	if err != nil {
		return nil, fmt.Errorf("list client_attachments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ClientAttachmentRow
	for rows.Next() {
		var a ClientAttachmentRow
		var kind, detached sql.NullString
		if err := rows.Scan(&a.ID, &a.SessionID, &kind, &a.AttachedAt, &detached); err != nil {
			return nil, fmt.Errorf("scan client_attachment: %w", err)
		}
		a.ClientKind, a.DetachedAt = kind.String, detached.String
		out = append(out, a)
	}
	return out, rows.Err()
}

// SweepStaleAttachments stamps detached_at on every row still attached, for once at daemon start
// (no attach survives a restart), and returns how many.
func (s *Store) SweepStaleAttachments(ctx context.Context, now string) (int, error) {
	n, err := s.execN(ctx, `UPDATE client_attachments SET detached_at = ? WHERE detached_at IS NULL`, now)
	if err != nil {
		return 0, fmt.Errorf("sweep client_attachments: %w", err)
	}
	return int(n), nil
}
