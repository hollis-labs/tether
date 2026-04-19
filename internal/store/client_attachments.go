package store

import (
	"database/sql"
	"fmt"
)

// ClientAttachmentRow mirrors the client_attachments table. Times are stored
// as RFC3339 strings to match the rest of the schema.
type ClientAttachmentRow struct {
	ID         string
	SessionID  string
	ClientKind string
	AttachedAt string
	DetachedAt sql.NullString
}

// CreateClientAttachment inserts a new attachment row. attachedAt must be an
// RFC3339 timestamp; the caller formats.
func (s *Store) CreateClientAttachment(id, sessionID, clientKind, attachedAt string) error {
	_, err := s.db.Exec(
		`INSERT INTO client_attachments (id, session_id, client_kind, attached_at) VALUES (?, ?, ?, ?)`,
		id, sessionID, clientKind, attachedAt,
	)
	if err != nil {
		return fmt.Errorf("insert client_attachment: %w", err)
	}
	return nil
}

// DetachClientAttachment stamps detached_at on the row with the given id.
// Idempotent: a second call with the same id just rewrites detached_at.
func (s *Store) DetachClientAttachment(id, detachedAt string) error {
	_, err := s.db.Exec(
		`UPDATE client_attachments SET detached_at=? WHERE id=?`,
		detachedAt, id,
	)
	if err != nil {
		return fmt.Errorf("update client_attachment: %w", err)
	}
	return nil
}

// ListClientAttachments returns attachments for a session in attached_at
// order (oldest first).
func (s *Store) ListClientAttachments(sessionID string) ([]ClientAttachmentRow, error) {
	rows, err := s.db.Query(
		`SELECT id, session_id, client_kind, attached_at, detached_at
         FROM client_attachments WHERE session_id=? ORDER BY attached_at`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("list client_attachments: %w", err)
	}
	defer rows.Close()
	var out []ClientAttachmentRow
	for rows.Next() {
		var r ClientAttachmentRow
		if err := rows.Scan(&r.ID, &r.SessionID, &r.ClientKind, &r.AttachedAt, &r.DetachedAt); err != nil {
			return nil, fmt.Errorf("scan client_attachment: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SweepStaleAttachments stamps detached_at on any row where it is still
// NULL. Intended for call-once-on-daemon-start: after a daemon restart, no
// live attach can still be running, so un-stamped rows are orphans. Returns
// the number of rows updated.
func (s *Store) SweepStaleAttachments(now string) (int, error) {
	res, err := s.db.Exec(
		`UPDATE client_attachments SET detached_at=? WHERE detached_at IS NULL`,
		now,
	)
	if err != nil {
		return 0, fmt.Errorf("sweep client_attachments: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}
