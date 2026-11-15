package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store"
)

var (
	// ErrSessionShimNotFound is returned for a session with no shim placement row.
	ErrSessionShimNotFound = errors.New("session shim not found")
	// ErrSessionShimConflict is returned when a write does not match the stored placement identity
	// (or would move its counters, epoch or cursor backwards).
	ErrSessionShimConflict = errors.New("session shim identity conflict")
)

// SessionShimRow is one session_shims row. It holds no capability values or launch env; the
// descriptor path names a private file whose ownership the host validates.
type SessionShimRow struct {
	SessionID           string
	ShimKey             string
	HostBackend         string
	UnitName            string
	SocketPath          string
	DescriptorPath      string
	JournalID           string
	Runtime             string
	RuntimeGeneration   uint64
	BootGeneration      string
	ControllerEpoch     uint64
	LastCommittedCursor string
	InjectCounter       uint64
	HostPID             int
	ShimPID             int
	ProviderPID         int
	CreatedAt           string
	UpdatedAt           string
}

const shimColumns = `session_id, shim_key, host_backend, unit_name, socket_path, descriptor_path, journal_id, runtime,
 runtime_generation, boot_generation, controller_epoch, last_committed_cursor, inject_counter, host_pid, shim_pid,
 provider_pid, created_at, updated_at`

// scanShim reads a row; the three counters are stored as decimal text (they are uint64).
func scanShim(scan func(...any) error) (SessionShimRow, error) {
	var row SessionShimRow
	var generation, epoch, counter string
	err := scan(&row.SessionID, &row.ShimKey, &row.HostBackend, &row.UnitName, &row.SocketPath, &row.DescriptorPath, &row.JournalID,
		&row.Runtime, &generation, &row.BootGeneration, &epoch, &row.LastCommittedCursor, &counter, &row.HostPID, &row.ShimPID,
		&row.ProviderPID, &row.CreatedAt, &row.UpdatedAt)
	if err != nil {
		return row, err
	}
	if row.RuntimeGeneration, err = strconv.ParseUint(generation, 10, 64); err != nil {
		return row, err
	}
	if row.ControllerEpoch, err = strconv.ParseUint(epoch, 10, 64); err != nil {
		return row, err
	}
	row.InjectCounter, err = strconv.ParseUint(counter, 10, 64)
	return row, err
}

// SessionShimTx reads the session's placement on q (a reader or the caller's transaction).
func SessionShimTx(ctx context.Context, q gostore.Querier, sessionID string) (SessionShimRow, error) {
	row, err := scanShim(q.QueryRowContext(ctx, rebind(q, `SELECT `+shimColumns+` FROM session_shims WHERE session_id = ?`), sessionID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return row, ErrSessionShimNotFound
	}
	return row, err
}

// SessionShim returns the session's placement (ErrSessionShimNotFound when none).
func (s *Store) SessionShim(ctx context.Context, sessionID string) (SessionShimRow, error) {
	return SessionShimTx(ctx, s.c.Reader(), sessionID)
}

// ListSessionShims returns every placement, historical ones included (reconciliation filters by
// session state at the daemon boundary rather than deleting evidence), by session id.
func (s *Store) ListSessionShims(ctx context.Context) ([]SessionShimRow, error) {
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, `SELECT `+shimColumns+` FROM session_shims ORDER BY session_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SessionShimRow
	for rows.Next() {
		row, err := scanShim(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// shimLockSession locks the session row inside a class C WriteTx on Postgres (SQLite's IMMEDIATE
// WriteTx already holds the one writer). Every shim writer takes it first, as the old writer
// reservation implied, so a placement write and a decision on the same session serialize
// (workspaces.go wsLockDecision locks the same row).
func shimLockSession(ctx context.Context, tx store.Tx, sessionID string) error {
	if tx.Dialect().Name() != gostore.DriverPostgres {
		return nil
	}
	rows, err := tx.QueryContext(ctx, rebind(tx, `SELECT id FROM sessions WHERE id = ? FOR UPDATE`), sessionID)
	if err != nil {
		return err
	}
	return rows.Close()
}

// UpsertSessionShim records a placement, or advances the stored one. Stable placement identity
// (key, backend, socket, descriptor, runtime, generations) and the original created_at are kept;
// a different journal is refused once one is recorded (journal replacement is explicit recovery,
// never an upsert); the inject counter, controller epoch and cursor position never move backwards.
// The read and the write run under the session row lock, so two upserts cannot both pass the
// monotonic check against the same old row.
func (s *Store) UpsertSessionShim(ctx context.Context, row SessionShimRow) error {
	if row.SessionID == "" || row.ShimKey == "" || row.Runtime == "" || row.BootGeneration == "" || row.RuntimeGeneration == 0 ||
		!filepath.IsAbs(row.SocketPath) || !filepath.IsAbs(row.DescriptorPath) || row.HostPID < 0 || row.ShimPID < 0 || row.ProviderPID < 0 {
		return fmt.Errorf("invalid session shim")
	}
	if row.HostBackend != "detached" && row.HostBackend != "systemd-user" {
		return fmt.Errorf("invalid shim host backend")
	}
	nextCursor, err := shimCursorPosition(row.JournalID, row.LastCommittedCursor)
	if err != nil {
		return err
	}
	return s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := shimLockSession(ctx, tx, row.SessionID); err != nil {
			return err
		}
		previous, err := SessionShimTx(ctx, tx, row.SessionID)
		switch {
		case errors.Is(err, ErrSessionShimNotFound):
			if found, err := shimSessionExists(ctx, tx, row.SessionID); err != nil {
				return err
			} else if !found {
				return ErrSessionNotFound
			}
		case err != nil:
			return err
		default:
			if previous.JournalID != "" && previous.JournalID != row.JournalID {
				return ErrSessionShimConflict
			}
			oldCursor, err := shimCursorPosition(previous.JournalID, previous.LastCommittedCursor)
			if err != nil {
				return err
			}
			if row.InjectCounter < previous.InjectCounter || row.ControllerEpoch < previous.ControllerEpoch || nextCursor < oldCursor {
				return ErrSessionShimConflict
			}
		}
		now := s.now().Format(time.RFC3339Nano)
		if row.CreatedAt == "" {
			row.CreatedAt = now
		}
		row.UpdatedAt = now
		res, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO session_shims (`+shimColumns+`)
 SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM sessions WHERE id = ?)
 ON CONFLICT (session_id) DO UPDATE SET
 unit_name = excluded.unit_name, journal_id = excluded.journal_id, controller_epoch = excluded.controller_epoch,
 last_committed_cursor = excluded.last_committed_cursor, inject_counter = excluded.inject_counter,
 host_pid = excluded.host_pid, shim_pid = excluded.shim_pid, provider_pid = excluded.provider_pid, updated_at = excluded.updated_at
 WHERE session_shims.shim_key = excluded.shim_key AND session_shims.host_backend = excluded.host_backend
 AND session_shims.socket_path = excluded.socket_path AND session_shims.descriptor_path = excluded.descriptor_path
 AND session_shims.runtime = excluded.runtime AND session_shims.runtime_generation = excluded.runtime_generation
 AND session_shims.boot_generation = excluded.boot_generation
 AND (session_shims.journal_id = '' OR session_shims.journal_id = excluded.journal_id)`),
			row.SessionID, row.ShimKey, row.HostBackend, row.UnitName, row.SocketPath, row.DescriptorPath, row.JournalID, row.Runtime,
			strconv.FormatUint(row.RuntimeGeneration, 10), row.BootGeneration, strconv.FormatUint(row.ControllerEpoch, 10),
			row.LastCommittedCursor, strconv.FormatUint(row.InjectCounter, 10), row.HostPID, row.ShimPID, row.ProviderPID,
			row.CreatedAt, row.UpdatedAt, row.SessionID)
		if err != nil {
			return fmt.Errorf("save session shim: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			if found, err := shimSessionExists(ctx, tx, row.SessionID); err != nil {
				return err
			} else if !found {
				return ErrSessionNotFound
			}
			return ErrSessionShimConflict
		}
		return nil
	})
}

func shimSessionExists(ctx context.Context, q gostore.Querier, sessionID string) (bool, error) {
	var found bool
	err := q.QueryRowContext(ctx, rebind(q, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id = ?)`), sessionID).Scan(&found)
	return found, err
}

// RemoveUnstartedSessionShim deletes pre-submit bookkeeping, only while no host or provider PID is
// recorded; the caller has positive evidence that no hosted child started.
func (s *Store) RemoveUnstartedSessionShim(ctx context.Context, sessionID, key string) error {
	return s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := shimLockSession(ctx, tx, sessionID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, rebind(tx, `DELETE FROM session_shims WHERE session_id = ? AND shim_key = ? AND host_pid = 0 AND provider_pid = 0`), sessionID, key)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return ErrSessionShimConflict
		}
		return nil
	})
}

// CompleteSessionShim records a shim session's terminal outcome. Whichever terminal outcome commits
// first is kept: recovery and stop may finish concurrently, and neither rewrites it (completed
// false for the loser). It runs as the joint "CompleteSessionShim" (the S9 composition adds its
// class A writes there); the change is for the caller's event emission.
func (s *Store) CompleteSessionShim(ctx context.Context, sessionID, state string, exit int) (SessionStateChange, bool, error) {
	change := SessionStateChange{SessionID: sessionID, To: state}
	if state != "completed" && state != "failed" && state != "killed" {
		return change, false, fmt.Errorf("invalid shim terminal state")
	}
	var completed bool
	err := s.all.Joint(ctx, "CompleteSessionShim", func(ctx context.Context, tx store.Tx) error {
		completed = false
		if err := shimLockSession(ctx, tx, sessionID); err != nil {
			return err
		}
		var agent sql.NullString
		if err := tx.QueryRowContext(ctx, rebind(tx, `SELECT state, logical_agent_id FROM sessions WHERE id = ?`), sessionID).Scan(&change.From, &agent); err != nil {
			return err
		}
		change.LogicalAgentID = agent.String
		now := s.secondStamp()
		res, err := tx.ExecContext(ctx, rebind(tx, `UPDATE sessions SET state = ?, pid = 0, exit_code = ?, updated_at = ?, ended_at = ?
 WHERE id = ? AND state IN ('created','launching','running','detached') AND EXISTS (SELECT 1 FROM session_shims WHERE session_id = ?)`),
			state, exit, now, now, sessionID, sessionID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		completed = n == 1
		return err
	})
	if err != nil {
		return change, false, err
	}
	return change, completed, nil
}

// shimCursorPosition parses a "<journal>:<position>" cursor ("" is position 0).
func shimCursorPosition(journal, cursor string) (uint64, error) {
	if cursor == "" {
		return 0, nil
	}
	i := strings.LastIndexByte(cursor, ':')
	if i < 0 {
		return 0, fmt.Errorf("invalid shim cursor")
	}
	prefix, position := cursor[:i], cursor[i+1:]
	n, err := strconv.ParseUint(position, 10, 64)
	if journal == "" || prefix != journal || err != nil || strconv.FormatUint(n, 10) != position {
		return 0, fmt.Errorf("invalid shim cursor")
	}
	return n, nil
}
