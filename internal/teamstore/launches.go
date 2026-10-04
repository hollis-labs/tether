package teamstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"

	"github.com/hollis-labs/substrate/mesh/teams"
)

// GetLaunch is a detached read; read-modify-write callers use WithLease to
// serialize intent recovery. Pure observation also works on read-only handles.
func (s *Store) GetLaunch(ctx context.Context, key string) (teams.LaunchRecord, error) {
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM team_launches WHERE launch_key=?`, key).Scan(&payload)
	if err != nil {
		return teams.LaunchRecord{}, readError("team launch", err)
	}
	var record teams.LaunchRecord
	err = decode(payload, &record)
	return record, err
}
func launchRank(state teams.LaunchState) int {
	switch state {
	case teams.Planning:
		return 1
	case teams.Prepared:
		return 2
	case teams.Launched:
		return 3
	case teams.MembersReady:
		return 4
	case teams.RoutingReady:
		return 5
	case teams.Aborting:
		return 6
	case teams.Failed:
		return 7
	default:
		return 0
	}
}
func validTransition(old, next teams.LaunchRecord) bool {
	if old.Digest != next.Digest || !reflect.DeepEqual(old.Team, next.Team) || !reflect.DeepEqual(old.Definition, next.Definition) || old.Limits != next.Limits || !old.Deadline.Equal(next.Deadline) || next.Attempts < old.Attempts || len(old.Intents) != len(next.Intents) {
		return false
	}
	for i, prior := range old.Intents {
		updated := next.Intents[i]
		if prior.Key != updated.Key || prior.MemberID != updated.MemberID || !reflect.DeepEqual(prior.Slot, updated.Slot) || !reflect.DeepEqual(prior.Request, updated.Request) || (prior.Cleaned && !updated.Cleaned) {
			return false
		}
		if prior.Member != nil && !reflect.DeepEqual(prior.Member, updated.Member) {
			return false
		}
	}
	if old.Run.ID != "" && (old.Run.ID != next.Run.ID || old.Run.TeamID != next.Run.TeamID || old.Run.TeamVersion != next.Run.TeamVersion || old.Run.Channel != next.Run.Channel) {
		return false
	}
	if old.State == teams.RoutingReady || old.State == teams.Failed {
		return reflect.DeepEqual(old, next)
	}
	if old.State == teams.Aborting {
		return next.State == teams.Aborting || next.State == teams.Failed
	}
	if next.State == teams.Aborting {
		return true
	}
	before, after := launchRank(old.State), launchRank(next.State)
	return before >= 1 && before <= 4 && after >= before && after <= before+1
}

// LaunchWriteHook writes related metadata using the supplied transaction
// connection and the same normalized record persisted by PutLaunch. It must not
// mutate the record, use the pool or call external ports. Errors roll back all
// launch, journal and related metadata writes. Configure before launch writes.
type LaunchWriteHook func(context.Context, *sql.Conn, teams.LaunchRecord) error

func (s *Store) SetLaunchWriteHook(hook LaunchWriteHook) {
	if hook == nil {
		s.launchWriteHook.Store(nil)
		return
	}
	s.launchWriteHook.Store(&hook)
}
func (s *Store) afterLaunchWrite(ctx context.Context, conn *sql.Conn, record teams.LaunchRecord, hook *LaunchWriteHook) error {
	if hook == nil {
		return nil
	}
	return (*hook)(ctx, conn, record)
}

// PutLaunch binds immutable intent and commits the record and journal together.
// Identical retries do not append journal entries. Every write checks the lease
// both before changing rows and before committing to fence expiry during a write.
func (s *Store) PutLaunch(ctx context.Context, record teams.LaunchRecord) error {
	token, err := launchToken(ctx, record.Key)
	if err != nil {
		return err
	}
	if record.Digest == "" || record.Team.ID == "" || record.Team.Version == 0 || record.Attempts < 0 || record.Deadline.IsZero() || launchRank(record.State) == 0 {
		return errors.New("team launch: incomplete record")
	}
	if err = record.Limits.Validate(); err != nil {
		return fmt.Errorf("launch limits: %w", err)
	}
	payload, err := encode(record)
	if err != nil {
		return err
	}
	var normalized teams.LaunchRecord
	if err = decode(payload, &normalized); err != nil {
		return err
	}
	record = normalized
	hook := s.launchWriteHook.Load()
	return s.immediate(ctx, func(conn *sql.Conn) error {
		if err := s.checkLease(ctx, conn, token); err != nil {
			return err
		}
		var stored []byte
		var revision uint64
		err := conn.QueryRowContext(ctx, `SELECT payload,revision FROM team_launches WHERE launch_key=?`, record.Key).Scan(&stored, &revision)
		if err == nil {
			var old teams.LaunchRecord
			if err = decode(stored, &old); err != nil {
				return err
			}
			if !validTransition(old, record) {
				return teams.ErrConflict
			}
			if reflect.DeepEqual(old, record) {
				if err := s.afterLaunchWrite(ctx, conn, record, hook); err != nil {
					return err
				}
				return s.checkLease(ctx, conn, token)
			}
			result, err := conn.ExecContext(ctx, `UPDATE team_launches SET payload=?,state=?,revision=revision+1 WHERE launch_key=? AND revision=?`, payload, record.State, record.Key, revision)
			if err != nil {
				return fmt.Errorf("update team launch: %w", err)
			}
			n, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("launch affected rows: %w", err)
			}
			if n != 1 {
				return teams.ErrConflict
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			if record.State != teams.Planning {
				return teams.ErrConflict
			}
			if _, err = conn.ExecContext(ctx, `INSERT INTO team_launches(launch_key,digest,state,revision,payload) VALUES(?,?,?,1,?)`, record.Key, record.Digest, record.State, payload); err != nil {
				return fmt.Errorf("insert team launch: %w", err)
			}
		} else {
			return fmt.Errorf("read launch before write: %w", err)
		}
		if _, err = conn.ExecContext(ctx, `INSERT INTO team_launch_journal(launch_key,revision,fence,payload) VALUES(?,?,?,?)`, record.Key, revision+1, token.fence, payload); err != nil {
			return fmt.Errorf("append launch journal: %w", err)
		}
		if err := s.afterLaunchWrite(ctx, conn, record, hook); err != nil {
			return err
		}
		return s.checkLease(ctx, conn, token)
	})
}

// Pending rotates lexically after the previous key, wrapping at the head.
func (s *Store) Pending(ctx context.Context, after string, limit int) ([]string, error) {
	if limit < 1 || limit > 1024 {
		return nil, errors.New("pending launch limit must be 1..1024")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT launch_key FROM team_launches WHERE state NOT IN (?,?) ORDER BY CASE WHEN launch_key>? THEN 0 ELSE 1 END,launch_key LIMIT ?`, teams.RoutingReady, teams.Failed, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending launches: %w", err)
	}
	defer func() { _ = rows.Close() }()
	keys := []string{}
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan pending launch: %w", err)
		}
		keys = append(keys, key)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending launches: %w", err)
	}
	return keys, nil
}

type JournalEntry struct {
	Revision uint64
	Fence    uint64
	Record   teams.LaunchRecord
}

// Journal reads a bounded immutable page after a per-launch revision cursor.
func (s *Store) Journal(ctx context.Context, key string, after uint64, limit int) ([]JournalEntry, error) {
	if limit < 1 || limit > 1024 {
		return nil, errors.New("launch journal limit must be 1..1024")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT revision,fence,payload FROM team_launch_journal WHERE launch_key=? AND revision>? ORDER BY revision LIMIT ?`, key, after, limit)
	if err != nil {
		return nil, fmt.Errorf("read launch journal: %w", err)
	}
	defer func() { _ = rows.Close() }()
	entries := []JournalEntry{}
	for rows.Next() {
		var entry JournalEntry
		var payload []byte
		if err = rows.Scan(&entry.Revision, &entry.Fence, &payload); err != nil {
			return nil, fmt.Errorf("scan launch journal: %w", err)
		}
		if err = decode(payload, &entry.Record); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate launch journal: %w", err)
	}
	return entries, nil
}
