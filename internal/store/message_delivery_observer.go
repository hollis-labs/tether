package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/hollis-labs/substrate/mesh/messaging"
)

type deliveryObserver struct {
	mu sync.RWMutex
	fn func(context.Context, messaging.Envelope) string
}

// SetMessageDeliveryObserver installs the recipient host's post-commit hook.
// It cannot roll back mail or turn a successful mail delivery into a failure.
func (s *Store) SetMessageDeliveryObserver(fn func(context.Context, messaging.Envelope) string) {
	d := s.MessagingStore().(*deliveryBackedStore)
	d.observer.mu.Lock()
	d.observer.fn = fn
	d.observer.mu.Unlock()
}

func (s *Store) HasMessageDeliveryObserver() bool {
	d := s.MessagingStore().(*deliveryBackedStore)
	d.observer.mu.RLock()
	defer d.observer.mu.RUnlock()
	return d.observer.fn != nil
}

func (d *deliveryBackedStore) observe(ctx context.Context, env messaging.Envelope) messaging.Envelope {
	d.observer.mu.RLock()
	fn := d.observer.fn
	d.observer.mu.RUnlock()
	if fn == nil {
		return env
	}
	outcome := fn(ctx, env)
	if outcome == "" {
		return env
	}
	meta := make(map[string]string, len(env.Metadata)+1)
	for k, v := range env.Metadata {
		meta[k] = v
	}
	meta["tether.wake_outcome"] = outcome
	env.Metadata = meta
	return env
}

// AdmitRecipientWake is an at-most-once admission per canonical message id.
// A crash after admission leaves an explicit unknown outcome; redelivery must
// not guess whether the provider received a turn and submit it again.
func (s *Store) AdmitRecipientWake(ctx context.Context, id string) (bool, string, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO recipient_wake_admissions(message_id,admitted_at) SELECT id,? FROM messages WHERE id=? ON CONFLICT(message_id) DO NOTHING`, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return false, "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, "", err
	}
	if n == 1 {
		return true, "", nil
	}
	var result sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT outcome_json FROM recipient_wake_admissions WHERE message_id=?`, id).Scan(&result)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", messaging.ErrNotFound
	}
	return false, result.String, err
}

func (s *Store) CompleteRecipientWake(ctx context.Context, id, outcome string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE recipient_wake_admissions SET outcome_json=? WHERE message_id=?`, outcome, id)
	return err
}

func (s *Store) RecipientWakeOutcome(ctx context.Context, id string) (string, bool, error) {
	var result sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT outcome_json FROM recipient_wake_admissions WHERE message_id=?`, id).Scan(&result)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return result.String, err == nil, err
}
