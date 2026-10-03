package teamstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/hollis-labs/substrate/mesh/teams"
)

// RecordSignal returns ErrNotFound until CreateRun has created the container.
func (s *Store) RecordSignal(ctx context.Context, record teams.PhaseSignalRecord) error {
	if record.RunID == "" || record.PhaseID == "" || record.Actor.Validate() != nil || record.RosterVersion == 0 {
		return errors.New("phase signal: incomplete record")
	}
	payload, err := encode(record)
	if err != nil {
		return err
	}
	return s.fencedImmediate(ctx, func(conn *sql.Conn) error {
		if err := requireRun(ctx, conn, record.RunID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO team_phase_signals(run_id,phase_id,actor,payload) VALUES(?,?,?,?) ON CONFLICT(run_id,phase_id,actor) DO NOTHING`, record.RunID, record.PhaseID, record.Actor, payload); err != nil {
			return fmt.Errorf("record phase signal: %w", err)
		}
		return nil
	})
}
func (s *Store) ListSignals(ctx context.Context, runID, phaseID string) ([]teams.PhaseSignalRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM team_phase_signals WHERE run_id=? AND phase_id=? ORDER BY sequence`, runID, phaseID)
	if err != nil {
		return nil, fmt.Errorf("list phase signals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	records := []teams.PhaseSignalRecord{}
	for rows.Next() {
		var record teams.PhaseSignalRecord
		var payload []byte
		if err = rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("scan phase signal: %w", err)
		}
		if err = decode(payload, &record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate phase signals: %w", err)
	}
	return records, nil
}
func (s *Store) GetSignal(ctx context.Context, runID, phaseID string) (teams.SignalResolution, error) {
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM team_signal_resolutions WHERE run_id=? AND phase_id=?`, runID, phaseID).Scan(&payload)
	if err != nil {
		return teams.SignalResolution{}, readError("phase resolution", err)
	}
	var record teams.SignalResolution
	err = decode(payload, &record)
	return record, err
}

// ResolveSignal returns ErrNotFound until CreateRun has created the container.
func (s *Store) ResolveSignal(ctx context.Context, record teams.SignalResolution) (teams.SignalResolution, error) {
	if record.RunID == "" || record.PhaseID == "" || record.Actor.Validate() != nil || record.Signaler.Validate() != nil || record.RosterVersion == 0 {
		return teams.SignalResolution{}, errors.New("phase resolution: incomplete record")
	}
	payload, err := encode(record)
	if err != nil {
		return teams.SignalResolution{}, err
	}
	var result teams.SignalResolution
	err = s.fencedImmediate(ctx, func(conn *sql.Conn) error {
		if err := requireRun(ctx, conn, record.RunID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO team_signal_resolutions(run_id,phase_id,payload) VALUES(?,?,?) ON CONFLICT(run_id,phase_id) DO NOTHING`, record.RunID, record.PhaseID, payload); err != nil {
			return fmt.Errorf("resolve phase signal: %w", err)
		}
		var stored []byte
		if err := conn.QueryRowContext(ctx, `SELECT payload FROM team_signal_resolutions WHERE run_id=? AND phase_id=?`, record.RunID, record.PhaseID).Scan(&stored); err != nil {
			return fmt.Errorf("read winning resolution: %w", err)
		}
		return decode(stored, &result)
	})
	return result, err
}
