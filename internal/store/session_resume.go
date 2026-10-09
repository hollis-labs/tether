package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// LatestSessionForAgent deliberately includes terminal rows: an ended process
// can still have a resumable provider conversation. rowid resolves timestamp
// ties without depending on UUID order.
func (s *Store) LatestSessionForAgent(ctx context.Context, agentID string) (*SessionRow, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM sessions WHERE logical_agent_id=? ORDER BY created_at DESC, rowid DESC LIMIT 1`, agentID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.GetSessionContext(ctx, id)
}

// AgentHasLiveSession prevents recovery from creating a competing child. An
// orphan has been proven gone; a detached or unreconciled child has not.
func (s *Store) AgentHasLiveSession(ctx context.Context, agentID string) (bool, error) {
	var live bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE logical_agent_id=? AND state IN ('created','ready','launching','running','detached'))`, agentID).Scan(&live)
	return live, err
}

// ClearNativeResume fences a failed native ID without erasing a newer provider
// observation. A retained NULL mapping is a tombstone: checkpoint hints must
// not resurrect the failed ID. The new session's persisted plan is cleared too,
// so a subsequent daemon recovery cannot repeat the same failed resume.
func (s *Store) ClearNativeResume(ctx context.Context, sourceID, sessionID, providerID, nativeID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Replacement lineage freezes the original conversation record. A failed
	// native attempt tombstones only the new execution's copied resume input.
	var replacement string
	err = tx.QueryRowContext(ctx, `SELECT replacement_session_id FROM session_replacements WHERE source_session_id=?`, sourceID).Scan(&replacement)
	if err == nil {
		if replacement != sessionID {
			return ErrSessionReplacementUnavailable
		}
		sourceID = sessionID
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO session_provider_mappings(session_id,owner,provider,native_session_id,created_at,updated_at)
		VALUES (?,'tether',?,NULL,?,?) ON CONFLICT(session_id,owner,provider) DO UPDATE SET native_session_id=NULL,updated_at=excluded.updated_at
		WHERE session_provider_mappings.native_session_id=?`, sourceID, providerID, time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339), nativeID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE launch_plans SET plan_json=json_remove(plan_json,'$.resume_provider_session_id') WHERE session_id=? AND json_extract(plan_json,'$.resume_provider_session_id')=?`, sessionID, nativeID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// SessionBootDir reads the exact historical plant event for launches that
// predate NativeStateRoot. Missing metadata never triggers a directory scan.
func (s *Store) SessionBootDir(ctx context.Context, id string) (string, error) {
	var root string
	err := s.db.QueryRowContext(ctx, `SELECT json_extract(payload_json,'$.path') FROM events WHERE session_id=? AND kind='session.boot_dir_planted' AND json_valid(payload_json) ORDER BY id DESC LIMIT 1`, id).Scan(&root)
	return root, err
}

func (s *Store) SetNativeStateRoot(ctx context.Context, id, root string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE launch_plans SET plan_json=json_set(plan_json,'$.native_state_root',?) WHERE session_id=?`, root, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrSessionNotFound
	}
	return err
}
