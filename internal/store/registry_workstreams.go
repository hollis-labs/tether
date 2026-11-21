package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

var ErrRegistryWorkstreamRequired = errors.New("task-linked workstream assignment must come from the Registry dispatcher")

// IsRegistryWorkstreamID recognizes the wire shape, not remote existence or
// authorization. Launch admission remains responsible for trusting the caller.
func IsRegistryWorkstreamID(id string) bool {
	const prefix = "workstream:"
	if !strings.HasPrefix(id, prefix) {
		return false
	}
	id = strings.TrimPrefix(id, prefix)
	if len(id) != 26 || id[0] > '7' {
		return false
	}
	for _, c := range id {
		if !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", c) {
			return false
		}
	}
	return true
}

// referenceRegistryWorkstreamTx retains the exact external identity as the
// local key. This is a reference row, not an authored copy of Registry metadata.
// Historical rows and their joins are never rewritten.
func referenceRegistryWorkstreamTx(ctx context.Context, tx *sql.Tx, id string) error {
	if !IsRegistryWorkstreamID(id) {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := tx.ExecContext(ctx, `INSERT INTO workstreams(id,status,created_at,updated_at) VALUES(?,'active',?,?) ON CONFLICT(id) DO NOTHING`, id, now, now)
	return err
}
