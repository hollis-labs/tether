package teamstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const RecoveryBackoffCap = time.Minute

// RecoveryReady checks durable retry state. A changed operation (notably cleanup
// replacing acquisition) runs immediately. Clock rollback is capped persistently.
// This schedules retries; callers still own effect locks or launch leases.
func (s *Store) RecoveryReady(ctx context.Context, kind, key, mode string, now time.Time) (bool, error) {
	ready := false
	err := s.WithTransaction(ctx, func(c *sql.Conn) error {
		var savedMode string
		var next int64
		err := c.QueryRowContext(ctx, `SELECT mode,next_attempt_at FROM team_recovery_attempts WHERE kind=? AND intent_key=?`, kind, key).Scan(&savedMode, &next)
		if errors.Is(err, sql.ErrNoRows) {
			ready = true
			return nil
		}
		if err != nil {
			return err
		}
		if savedMode != mode {
			ready = true
			return nil
		}
		capAt := now.Add(RecoveryBackoffCap).UnixNano()
		if next > capAt {
			if _, err = c.ExecContext(ctx, `UPDATE team_recovery_attempts SET next_attempt_at=? WHERE kind=? AND intent_key=?`, capAt, kind, key); err != nil {
				return err
			}
			next = capAt
		}
		ready = next <= now.UnixNano()
		return nil
	})
	return ready, err
}

// FinishRecovery resets successful retries and returns an error only when the
// failure changes. Persisting a hash avoids copying error details into this table
// and keeps repeated failures quiet across daemon restarts.
func (s *Store) FinishRecovery(ctx context.Context, kind, key, mode string, now time.Time, cause error) error {
	changed := false
	err := s.WithTransaction(ctx, func(c *sql.Conn) error {
		if cause == nil {
			_, err := c.ExecContext(ctx, `DELETE FROM team_recovery_attempts WHERE kind=? AND intent_key=?`, kind, key)
			return err
		}
		var priorMode, priorHash string
		var attempts int
		err := c.QueryRowContext(ctx, `SELECT mode,attempts,error_hash FROM team_recovery_attempts WHERE kind=? AND intent_key=?`, kind, key).Scan(&priorMode, &attempts, &priorHash)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if mode != priorMode {
			attempts = 0
		}
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(cause.Error())))
		changed = digest != priorHash || mode != priorMode
		// Saturation bounds both arithmetic and the delay calculation.
		if attempts < 7 {
			attempts++
		}
		delay := time.Second << (attempts - 1)
		if delay > RecoveryBackoffCap {
			delay = RecoveryBackoffCap
		}
		_, err = c.ExecContext(ctx, `INSERT INTO team_recovery_attempts(kind,intent_key,mode,attempts,next_attempt_at,error_hash) VALUES(?,?,?,?,?,?) ON CONFLICT(kind,intent_key) DO UPDATE SET mode=excluded.mode,attempts=excluded.attempts,next_attempt_at=excluded.next_attempt_at,error_hash=excluded.error_hash`, kind, key, mode, attempts, now.Add(delay).UnixNano(), digest)
		return err
	})
	if err != nil {
		return errors.Join(cause, err)
	}
	if changed {
		return fmt.Errorf("team recovery %s %s: %w", kind, key, cause)
	}
	return nil
}
