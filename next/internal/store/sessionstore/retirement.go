package sessionstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store"
)

// The shim retirement and retention-cursor records are the retirement policy's (shimretire, S9)
// receipts, stored as JSON with a revision token. The store keeps them as rows: it bounds what it
// reads, writes a new record only under the revision the caller read (compare-and-set), and leaves
// decoding, validation and phase rules to the policy. Neither record ever holds a launch
// descriptor, provider env, journal output or bridge carry.

// MaxShimRecordBytes bounds a stored retirement or cursor record (the schema checks the same bound).
const MaxShimRecordBytes = 262144

var (
	// ErrShimRetirementConflict is returned when a record write does not match the stored revision,
	// digest or session, or a stored record exceeds MaxShimRecordBytes.
	ErrShimRetirementConflict = errors.New("shim retirement revision conflict")
	// ErrShimRecordNotFound is returned for an unknown operation or cursor id.
	ErrShimRecordNotFound = errors.New("shim retirement record not found")
)

// NewShimRevision mints a revision token for a record write (32 random bytes, hex), as the old
// store did inside Record: the caller embeds it in the record JSON and passes it as the new revision.
func NewShimRevision() (string, error) {
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", errors.New("retirement revision unavailable")
	}
	return hex.EncodeToString(nonce[:]), nil
}

// ShimRetirementRecord is one session_shim_retirements row.
type ShimRetirementRecord struct {
	OperationID, SessionID, RequestDigest, Revision, Phase string
	RecordJSON                                             string
	RetiredAt                                              string // "" when NULL
}

// shimBounded is the record column when it is within MaxShimRecordBytes, NULL otherwise, so an
// oversized value is never fetched into the process.
func shimBounded(q gostore.Querier, column string) string {
	if q.Dialect().Name() == gostore.DriverPostgres {
		return `CASE WHEN octet_length(` + column + `) <= ` + strconv.Itoa(MaxShimRecordBytes) + ` THEN ` + column + ` END`
	}
	return `CASE WHEN length(CAST(` + column + ` AS BLOB)) <= ` + strconv.Itoa(MaxShimRecordBytes) + ` THEN ` + column + ` END`
}

// ShimRetirementTx reads an operation's record on q (a reader or the caller's transaction):
// ErrShimRecordNotFound when none, ErrShimRetirementConflict when the stored record is oversized.
func ShimRetirementTx(ctx context.Context, q gostore.Querier, operationID string) (ShimRetirementRecord, error) {
	r := ShimRetirementRecord{OperationID: operationID}
	if operationID == "" {
		return r, ErrShimRetirementConflict
	}
	var raw, retired sql.NullString
	err := q.QueryRowContext(ctx, rebind(q, `SELECT `+shimBounded(q, "record_json")+`, revision, request_digest, session_id, phase, retired_at
 FROM session_shim_retirements WHERE operation_id = ?`), operationID).Scan(&raw, &r.Revision, &r.RequestDigest, &r.SessionID, &r.Phase, &retired)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrShimRecordNotFound
	}
	if err != nil {
		return r, errors.New("retirement receipt unavailable")
	}
	if !raw.Valid {
		return r, ErrShimRetirementConflict
	}
	r.RecordJSON, r.RetiredAt = raw.String, retired.String
	return r, nil
}

// ShimRetirement reads an operation's record.
func (s *Store) ShimRetirement(ctx context.Context, operationID string) (ShimRetirementRecord, error) {
	return ShimRetirementTx(ctx, s.c.Reader(), operationID)
}

// RecordShimRetirement writes rec (whose Revision is the new token, embedded in RecordJSON).
// expected "" inserts the first record, only for an existing session and only if the operation has
// none; otherwise the stored record is replaced only while it still has revision expected and the
// same request digest and session. ErrShimRetirementConflict when the write does not apply.
func (s *Store) RecordShimRetirement(ctx context.Context, rec ShimRetirementRecord, expected string) error {
	if rec.OperationID == "" || rec.Revision == "" || rec.Revision == expected || len(rec.RecordJSON) > MaxShimRecordBytes {
		return ErrShimRetirementConflict
	}
	now := s.now().Format(time.RFC3339Nano)
	return s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		var res sql.Result
		var err error
		if expected == "" {
			res, err = tx.ExecContext(ctx, rebind(tx, `INSERT INTO session_shim_retirements
 (operation_id, session_id, request_digest, revision, phase, record_json, retired_at, created_at, updated_at)
 SELECT ?, ?, ?, ?, ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM sessions WHERE id = ?) ON CONFLICT (operation_id) DO NOTHING`),
				rec.OperationID, rec.SessionID, rec.RequestDigest, rec.Revision, rec.Phase, rec.RecordJSON, nullText(rec.RetiredAt), now, now, rec.SessionID)
		} else {
			res, err = tx.ExecContext(ctx, rebind(tx, `UPDATE session_shim_retirements SET revision = ?, phase = ?, record_json = ?, retired_at = ?, updated_at = ?
 WHERE operation_id = ? AND revision = ? AND request_digest = ? AND session_id = ?`),
				rec.Revision, rec.Phase, rec.RecordJSON, nullText(rec.RetiredAt), now, rec.OperationID, expected, rec.RequestDigest, rec.SessionID)
		}
		if err != nil {
			return errors.New("retirement receipt commit uncertain")
		}
		if n, err := res.RowsAffected(); err != nil {
			return errors.New("retirement receipt commit uncertain")
		} else if n != 1 {
			return ErrShimRetirementConflict
		}
		return nil
	})
}

// ShimPlacementExpect is the placement a retirement receipt was issued for, as the retirement
// policy decodes it from the record: ReconcileShimRetirement requires the stored shim row to match.
type ShimPlacementExpect struct {
	ShimKey     string
	Generation  uint64
	HostBackend string
	UnitName    string
	JournalID   string
}

// ReconcileShimRetirement revokes a retired shim session's authority and records it orphaned, only
// against a bound durable cleanup receipt. It runs as the joint "ReconcileRetirement" (sessions and
// runtime_bindings are class C, principals class A): under the session row lock it reads the
// operation's record and hands it to check (the policy decodes and validates it, requires the
// cleanup phase and returns the placement it binds), then requires the session's shim row to match
// that placement and the session to be launching, running, detached or orphaned. It never invents
// a provider exit or ended_at. The caller still holds lifecycle, controller and placement exclusion.
func (s *Store) ReconcileShimRetirement(ctx context.Context, sessionID, operationID string,
	check func(ShimRetirementRecord) (ShimPlacementExpect, error)) (SessionStateChange, error) {
	change := SessionStateChange{SessionID: sessionID, To: "orphaned", Reason: "explicit_retirement"}
	if sessionID == "" || check == nil {
		return change, ErrShimRetirementConflict
	}
	err := s.all.Joint(ctx, "ReconcileRetirement", func(ctx context.Context, tx store.Tx) error {
		if err := shimLockSession(ctx, tx, sessionID); err != nil {
			return err
		}
		rec, err := ShimRetirementTx(ctx, tx, operationID)
		if err != nil || rec.SessionID != sessionID {
			return ErrShimRetirementConflict
		}
		want, err := check(rec)
		if err != nil {
			return ErrShimRetirementConflict
		}
		shim, err := SessionShimTx(ctx, tx, sessionID)
		if err != nil || shim.ShimKey != want.ShimKey || shim.RuntimeGeneration != want.Generation || shim.HostBackend != want.HostBackend ||
			shim.UnitName != want.UnitName || shim.JournalID != want.JournalID {
			return ErrShimRetirementConflict
		}
		var agent sql.NullString
		if err := tx.QueryRowContext(ctx, rebind(tx, `SELECT state, logical_agent_id FROM sessions WHERE id = ?`), sessionID).Scan(&change.From, &agent); err != nil {
			return ErrShimRetirementConflict
		}
		change.LogicalAgentID = agent.String
		switch change.From {
		case "launching", "running", "detached", "orphaned":
		default:
			return ErrShimRetirementConflict
		}
		now := s.polMillis()
		for _, st := range []struct {
			q    string
			args []any
		}{
			{`UPDATE sessions SET state = 'orphaned', pid = NULL, pid_started_at = NULL, updated_at = ? WHERE id = ?`, []any{now, sessionID}},
			{`UPDATE principals SET revoked_at = COALESCE(revoked_at, ?) WHERE kind = 'session' AND session_id = ?`, []any{now, sessionID}},
			{`UPDATE runtime_bindings SET revoked_at = ?, updated_at = ? WHERE session_id = ? AND revoked_at IS NULL`, []any{now, now, sessionID}},
		} {
			if _, err := tx.ExecContext(ctx, rebind(tx, st.q), st.args...); err != nil {
				return errors.New("retirement reconciliation unavailable")
			}
		}
		return nil
	})
	return change, err
}

// ShimRetentionCursor is one session_shim_retention_cursors row.
type ShimRetentionCursor struct {
	CursorID, RequestDigest, Revision, RecordJSON string
}

// ShimRetentionCursorTx reads a cursor on q: ErrShimRecordNotFound when none,
// ErrShimRetirementConflict when the stored record is oversized.
func ShimRetentionCursorTx(ctx context.Context, q gostore.Querier, cursorID string) (ShimRetentionCursor, error) {
	c := ShimRetentionCursor{CursorID: cursorID}
	var raw sql.NullString
	err := q.QueryRowContext(ctx, rebind(q, `SELECT `+shimBounded(q, "record_json")+`, request_digest, revision
 FROM session_shim_retention_cursors WHERE cursor_id = ?`), cursorID).Scan(&raw, &c.RequestDigest, &c.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrShimRecordNotFound
	}
	if err != nil {
		return c, errors.New("retention cursor unavailable")
	}
	if !raw.Valid {
		return c, ErrShimRetirementConflict
	}
	c.RecordJSON = raw.String
	return c, nil
}

// ShimRetentionCursor reads a cursor.
func (s *Store) ShimRetentionCursor(ctx context.Context, cursorID string) (ShimRetentionCursor, error) {
	return ShimRetentionCursorTx(ctx, s.c.Reader(), cursorID)
}

// RecordShimRetentionCursor writes c (whose Revision is the new token): expected "" inserts the
// first record if the cursor has none; otherwise it replaces the stored one only while it still has
// revision expected and the same request digest.
func (s *Store) RecordShimRetentionCursor(ctx context.Context, c ShimRetentionCursor, expected string) error {
	if c.CursorID == "" || c.Revision == "" || c.Revision == expected || len(c.RecordJSON) > MaxShimRecordBytes {
		return ErrShimRetirementConflict
	}
	now := s.now().Format(time.RFC3339Nano)
	return s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		var res sql.Result
		var err error
		if expected == "" {
			res, err = tx.ExecContext(ctx, rebind(tx, `INSERT INTO session_shim_retention_cursors (cursor_id, request_digest, revision, record_json, created_at, updated_at)
 VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (cursor_id) DO NOTHING`), c.CursorID, c.RequestDigest, c.Revision, c.RecordJSON, now, now)
		} else {
			res, err = tx.ExecContext(ctx, rebind(tx, `UPDATE session_shim_retention_cursors SET revision = ?, record_json = ?, updated_at = ?
 WHERE cursor_id = ? AND revision = ? AND request_digest = ?`), c.Revision, c.RecordJSON, now, c.CursorID, expected, c.RequestDigest)
		}
		if err != nil {
			return errors.New("retention cursor commit uncertain")
		}
		if n, err := res.RowsAffected(); err != nil {
			return errors.New("retention cursor commit uncertain")
		} else if n != 1 {
			return ErrShimRetirementConflict
		}
		return nil
	})
}
