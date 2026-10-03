package teamstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/hollis-labs/substrate/mesh/teams"
)

// ErrLeaseLost means an expired or superseded callback cannot mutate storage.
var ErrLeaseLost = errors.New("team launch lease lost")
var errLeaseBusy = errors.New("team launch lease busy")

type leaseContextKey struct{}
type leaseToken struct {
	key, owner string
	fence      uint64
}

func (s *Store) acquire(ctx context.Context, key, owner string) (leaseToken, error) {
	token := leaseToken{key: key, owner: owner}
	err := s.immediate(ctx, func(conn *sql.Conn) error {
		now := s.now()
		err := conn.QueryRowContext(ctx, `INSERT INTO team_launch_leases(launch_key,owner,fence,expires_ns) VALUES(?,?,1,?)
   ON CONFLICT(launch_key) DO UPDATE SET owner=excluded.owner,fence=team_launch_leases.fence+1,expires_ns=excluded.expires_ns
   WHERE team_launch_leases.expires_ns<=? RETURNING fence`, key, owner, now.Add(s.leaseDuration).UnixNano(), now.UnixNano()).Scan(&token.fence)
		if errors.Is(err, sql.ErrNoRows) {
			return errLeaseBusy
		}
		if err != nil {
			return fmt.Errorf("acquire launch lease: %w", err)
		}
		return nil
	})
	return token, err
}

// WithLease waits for a durable keyed lease, then supplies its fencing authority
// through the callback context. Storage mutations reject expired and superseded
// tokens in their transaction. PutLaunch additionally requires a matching key.
// There is no implicit renewal: long operations must reconcile under a new lease.
func (s *Store) WithLease(ctx context.Context, key string, fn func(context.Context) error) (retErr error) {
	if key == "" || fn == nil {
		return errors.New("launch lease: key and callback required")
	}
	owner := uuid.NewString()
	var token leaseToken
	for {
		var err error
		token, err = s.acquire(ctx, key, owner)
		if err == nil {
			break
		}
		if !errors.Is(err, errLeaseBusy) {
			return err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	leaseCtx, cancel := context.WithTimeout(context.WithValue(ctx, leaseContextKey{}, token), s.leaseDuration)
	defer cancel()
	// Release even when the callback panics; the fence predicates prevent a stale
	// callback from releasing the next holder's lease.
	defer func() {
		releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer releaseCancel()
		if _, err := s.db.ExecContext(releaseCtx, `UPDATE team_launch_leases SET owner='',expires_ns=0 WHERE launch_key=? AND owner=? AND fence=?`, token.key, token.owner, token.fence); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release launch lease: %w", err))
		}
	}()
	if err := fn(leaseCtx); err != nil {
		return err
	}
	if err := leaseCtx.Err(); err != nil {
		return err
	}
	// A host-provided clock may advance before the real context timer fires.
	return s.checkLease(leaseCtx, s.db, token)
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) checkLease(ctx context.Context, q rowQuerier, token leaseToken) error {
	var fence uint64
	err := q.QueryRowContext(ctx, `SELECT fence FROM team_launch_leases WHERE launch_key=? AND owner=? AND fence=? AND expires_ns>?`, token.key, token.owner, token.fence, s.now().UnixNano()).Scan(&fence)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("launch key %q: %w", token.key, ErrLeaseLost)
	}
	if err != nil {
		return fmt.Errorf("verify launch lease: %w", err)
	}
	return nil
}
func launchToken(ctx context.Context, key string) (leaseToken, error) {
	token, ok := ctx.Value(leaseContextKey{}).(leaseToken)
	if !ok || token.key != key {
		return leaseToken{}, fmt.Errorf("launch write requires matching lease: %w", teams.ErrDenied)
	}
	return token, nil
}
