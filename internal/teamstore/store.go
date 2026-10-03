// Package teamstore implements durable team storage over the daemon's SQLite
// database. It does not launch agents, create channels or expose public verbs.
package teamstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"time"

	"github.com/hollis-labs/substrate/mesh/teams"
)

// Options configures the lease lifetime and clock. Set once before use; Now
// must be safe for concurrent calls and shared by hosts using the same database.
type Options struct {
	LeaseDuration time.Duration
	Now           func() time.Time
}

// Store borrows a migrated database handle; its owner manages Close.
type Store struct {
	db            *sql.DB
	leaseDuration time.Duration
	now           func() time.Time
}

// New attaches storage without migrating or writing, including read-only handles.
func New(db *sql.DB, options Options) (*Store, error) {
	if db == nil {
		return nil, errors.New("team store: database required")
	}
	if options.LeaseDuration < 0 {
		return nil, errors.New("team store: negative lease duration")
	}
	if options.LeaseDuration == 0 {
		options.LeaseDuration = 30 * time.Second
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Store{db: db, leaseDuration: options.LeaseDuration, now: options.Now}, nil
}

// immediate serializes a callback across database handles before reading mutable
// state. The explicit write lock avoids deferred-transaction snapshot upgrades.
// Callbacks must use conn, not the pool (the daemon has one connection).
func (s *Store) immediate(ctx context.Context, fn func(*sql.Conn) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("team transaction connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("team transaction begin: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK") }()
	if err = fn(conn); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("team transaction commit: %w", err)
	}
	return nil
}

// fencedImmediate checks optional launch authority in the same transaction as
// the mutation, including expiry while the callback executes.
func (s *Store) fencedImmediate(ctx context.Context, fn func(*sql.Conn) error) error {
	return s.immediate(ctx, func(conn *sql.Conn) error {
		token, leased := ctx.Value(leaseContextKey{}).(leaseToken)
		if leased {
			if err := s.checkLease(ctx, conn, token); err != nil {
				return err
			}
		}
		if err := fn(conn); err != nil {
			return err
		}
		if leased {
			return s.checkLease(ctx, conn, token)
		}
		return nil
	})
}

var runName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,58}$`)

// ChannelName names the implicit run channel without creating it. Rejects names
// that cannot fit the channel grammar instead of truncating distinct identities.
func ChannelName(runID string) (string, error) {
	if !runName.MatchString(runID) {
		return "", errors.New("team channel: invalid run identifier")
	}
	return "team." + runID, nil
}

type RunContainer struct {
	Run            teams.TeamRun
	SessionGroupID string
}

// CreateRun atomically gives the run its own session group. Repeated identical
// input recovers the container; a different immutable definition conflicts.
// Run.Channel remains library metadata; the session group name is derived from
// the run ID. LaunchWorkflow must create this container before roster/signals
// can be written. Run status is immutable here.
func (s *Store) CreateRun(ctx context.Context, run teams.TeamRun) (RunContainer, error) {
	channel, err := ChannelName(run.ID)
	if err != nil {
		return RunContainer{}, err
	}
	if run.TeamID == "" || run.TeamVersion == 0 || !run.Status.Valid() {
		return RunContainer{}, errors.New("team run: definition and valid status required")
	}
	payload, err := encode(run)
	if err != nil {
		return RunContainer{}, err
	}
	var normalized teams.TeamRun
	if err = decode(payload, &normalized); err != nil {
		return RunContainer{}, err
	}
	result := RunContainer{Run: normalized, SessionGroupID: channel}
	err = s.fencedImmediate(ctx, func(conn *sql.Conn) error {
		var stored []byte
		err := conn.QueryRowContext(ctx, `SELECT payload FROM team_runs WHERE run_id=?`, run.ID).Scan(&stored)
		if err == nil {
			var old teams.TeamRun
			if err := decode(stored, &old); err != nil {
				return err
			}
			if !reflect.DeepEqual(old, normalized) {
				return teams.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read team run: %w", err)
		}
		var definition []byte
		if err = conn.QueryRowContext(ctx, `SELECT payload FROM team_definitions WHERE team_id=? AND version=?`, run.TeamID, run.TeamVersion).Scan(&definition); err != nil {
			return readError("run definition", err)
		}
		now := s.now().UTC().Format(time.RFC3339)
		// Foreign keys are not enforced by the daemon, so exclusive ownership is
		// checked here as well as by the run table's unique group constraint.
		var group string
		err = conn.QueryRowContext(ctx, `SELECT id FROM session_groups WHERE id=?`, channel).Scan(&group)
		if err == nil {
			return teams.ErrConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read run group: %w", err)
		}
		if _, err = conn.ExecContext(ctx, `INSERT INTO session_groups(id,name,workflow_id,created_at,updated_at) VALUES(?,?,?,?,?)`, channel, channel, run.ID, now, now); err != nil {
			return fmt.Errorf("create run group: %w", err)
		}
		if _, err = conn.ExecContext(ctx, `INSERT INTO team_runs(run_id,session_group_id,team_id,team_version,payload,created_at) VALUES(?,?,?,?,?,?)`, run.ID, channel, run.TeamID, run.TeamVersion, payload, now); err != nil {
			return fmt.Errorf("create team run: %w", err)
		}
		return nil
	})
	if err != nil {
		return RunContainer{}, fmt.Errorf("create team run: %w", err)
	}
	return result, nil
}
func (s *Store) GetRun(ctx context.Context, id string) (RunContainer, error) {
	var result RunContainer
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT session_group_id,payload FROM team_runs WHERE run_id=?`, id).Scan(&result.SessionGroupID, &payload)
	if err != nil {
		return result, readError("team run", err)
	}
	err = decode(payload, &result.Run)
	return result, err
}

func requireRun(ctx context.Context, conn *sql.Conn, id string) error {
	var found string
	err := conn.QueryRowContext(ctx, `SELECT run_id FROM team_runs WHERE run_id=?`, id).Scan(&found)
	return readError("team run", err)
}
func readError(scope string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", scope, teams.ErrNotFound)
	}
	return fmt.Errorf("%s: %w", scope, err)
}

var (
	_ teams.DefinitionStore = (*Store)(nil)
	_ teams.RosterStore     = (*Store)(nil)
	_ teams.LaunchLedger    = (*Store)(nil)
	_ teams.SignalStore     = (*Store)(nil)
)
