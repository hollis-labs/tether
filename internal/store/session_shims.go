package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"time"
)

var ErrSessionShimNotFound = errors.New("session shim not found")
var ErrSessionShimConflict = errors.New("session shim identity conflict")

// SessionShimRow excludes capability values and launch env. A descriptor path
// refers to a private file whose ownership/permissions the host validates.
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

const shimColumns = `session_id,shim_key,host_backend,unit_name,socket_path,descriptor_path,journal_id,runtime,runtime_generation,boot_generation,controller_epoch,last_committed_cursor,inject_counter,host_pid,shim_pid,provider_pid,created_at,updated_at`

// UpsertSessionShim retains stable placement identity and the original creation
// timestamp. Journal replacement must be explicit recovery, never an upsert.
func (s *Store) UpsertSessionShim(ctx context.Context, row SessionShimRow) error {
	if row.SessionID == "" || row.ShimKey == "" || row.Runtime == "" || row.BootGeneration == "" || row.RuntimeGeneration == 0 || !filepath.IsAbs(row.SocketPath) || !filepath.IsAbs(row.DescriptorPath) || row.HostPID < 0 || row.ShimPID < 0 || row.ProviderPID < 0 {
		return fmt.Errorf("invalid session shim")
	}
	if row.HostBackend != "detached" && row.HostBackend != "systemd-user" {
		return fmt.Errorf("invalid shim host backend")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if row.CreatedAt == "" {
		row.CreatedAt = now
	}
	row.UpdatedAt = now
	result, err := s.db.ExecContext(ctx, `INSERT INTO session_shims (`+shimColumns+`)
 SELECT ?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,? WHERE EXISTS (SELECT 1 FROM sessions WHERE id=?)
 ON CONFLICT(session_id) DO UPDATE SET
 unit_name=excluded.unit_name, journal_id=excluded.journal_id,
 controller_epoch=excluded.controller_epoch,last_committed_cursor=excluded.last_committed_cursor,
 inject_counter=excluded.inject_counter,host_pid=excluded.host_pid,shim_pid=excluded.shim_pid,
 provider_pid=excluded.provider_pid,updated_at=excluded.updated_at
 WHERE session_shims.shim_key=excluded.shim_key AND session_shims.host_backend=excluded.host_backend
 AND session_shims.socket_path=excluded.socket_path AND session_shims.descriptor_path=excluded.descriptor_path
 AND session_shims.runtime=excluded.runtime AND session_shims.runtime_generation=excluded.runtime_generation
 AND session_shims.boot_generation=excluded.boot_generation
 AND (session_shims.journal_id='' OR session_shims.journal_id=excluded.journal_id)`,
		row.SessionID, row.ShimKey, row.HostBackend, row.UnitName, row.SocketPath, row.DescriptorPath, row.JournalID, row.Runtime, strconv.FormatUint(row.RuntimeGeneration, 10), row.BootGeneration, strconv.FormatUint(row.ControllerEpoch, 10), row.LastCommittedCursor, strconv.FormatUint(row.InjectCounter, 10), row.HostPID, row.ShimPID, row.ProviderPID, row.CreatedAt, row.UpdatedAt, row.SessionID)
	if err != nil {
		return fmt.Errorf("save session shim: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var found int
		err = s.db.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id=?`, row.SessionID).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSessionNotFound
		}
		if err != nil {
			return err
		}
		return ErrSessionShimConflict
	}
	return nil
}
func scanShim(scanner interface{ Scan(...any) error }) (SessionShimRow, error) {
	var row SessionShimRow
	var generation, epoch, counter string
	err := scanner.Scan(&row.SessionID, &row.ShimKey, &row.HostBackend, &row.UnitName, &row.SocketPath, &row.DescriptorPath, &row.JournalID, &row.Runtime, &generation, &row.BootGeneration, &epoch, &row.LastCommittedCursor, &counter, &row.HostPID, &row.ShimPID, &row.ProviderPID, &row.CreatedAt, &row.UpdatedAt)
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
func (s *Store) SessionShim(ctx context.Context, id string) (SessionShimRow, error) {
	row, err := scanShim(s.db.QueryRowContext(ctx, `SELECT `+shimColumns+` FROM session_shims WHERE session_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return row, ErrSessionShimNotFound
	}
	return row, err
}

// ListSessionShims includes historical placements. Reconciliation applies the
// session state filter at the daemon boundary rather than deleting evidence.
func (s *Store) ListSessionShims(ctx context.Context) ([]SessionShimRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+shimColumns+` FROM session_shims ORDER BY session_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []SessionShimRow
	for rows.Next() {
		row, err := scanShim(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}
