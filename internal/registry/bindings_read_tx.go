package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CurrentBindingTx uses the same highest-live-generation predicate as the
// existing CurrentBinding. It does not renew, revoke or mint authority.
func CurrentBindingTx(ctx context.Context, tx *sql.Tx, targetURN string) (RuntimeBinding, error) {
	if tx == nil {
		return RuntimeBinding{}, fmt.Errorf("registry transaction required")
	}
	b, err := scanBinding(ctx, tx, `SELECT `+bindingColumns+` FROM runtime_bindings
 WHERE target_urn = ? AND revoked_at IS NULL
 AND (lease_expires_at IS NULL OR lease_expires_at > ?)
 ORDER BY generation DESC LIMIT 1`, targetURN, formatTime(time.Now().UTC()))
	if errors.Is(err, sql.ErrNoRows) {
		return RuntimeBinding{}, fmt.Errorf("%w: target=%s", ErrBindingNotFound, targetURN)
	}
	return b, err
}
