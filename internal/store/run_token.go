package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

var ErrRunTokenTarget = errors.New("invalid run token revocation identifier")

func ValidRunTokenTarget(id string) bool {
	return id != "" && len(id) <= 512 && !strings.ContainsAny(id, "\r\n\x00 .")
}

func (s *Store) RevokeRunToken(ctx context.Context, id string) error {
	if !ValidRunTokenTarget(id) {
		return ErrRunTokenTarget
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO run_token_denylist(target,revoked_at) VALUES (?,?) ON CONFLICT(target) DO NOTHING`, id, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) RunTokenDenied(ctx context.Context, jti, run string) (bool, error) {
	var denied bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM run_token_denylist WHERE target IN (?,?))`, jti, run).Scan(&denied)
	return denied, err
}
