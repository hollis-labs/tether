package teamruntime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/teamhost"
)

const pendingPortQuery = `SELECT port_kind,intent_key,request,ended,binding_ended,sequence FROM team_port_intents WHERE port_kind<>'delivery' AND state IN ('pending','done') AND (state='pending' OR ended<>'' OR binding_ended=1 OR (port_kind='session' AND EXISTS(SELECT 1 FROM session_idempotency k JOIN sessions s ON s.id=k.session_id WHERE k.key='team-port:'||nonce AND s.state='created'))) ORDER BY CASE WHEN sequence>? THEN 0 ELSE 1 END,sequence LIMIT ?`

// Reconciler repairs only committed write-ahead receipts. Construction starts
// no work; unreceipted namespaced objects belong to other callers.
type Reconciler struct {
	Enroller  teamhost.Enroller
	Sessions  *Sessions
	Messenger *Messenger
}

func (r Reconciler) Reconcile(ctx context.Context, limit int) error {
	if r.Enroller == nil || r.Sessions == nil || r.Messenger == nil || limit < 1 {
		return errors.New("team port recovery: adapters and positive limit required")
	}
	receipts := r.Sessions.receipts
	type pending struct {
		kind, key, end string
		request        []byte
		binding        bool
		sequence       int64
	}
	var entries []pending
	err := receipts.store.WithTransaction(ctx, func(conn *sql.Conn) error {
		var cursor int64
		err := conn.QueryRowContext(ctx, `SELECT position FROM team_host_recovery_cursors WHERE kind='ports'`).Scan(&cursor)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		rows, err := conn.QueryContext(ctx, pendingPortQuery, cursor, limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var entry pending
			if err = rows.Scan(&entry.kind, &entry.key, &entry.request, &entry.end, &entry.binding, &entry.sequence); err != nil {
				_ = rows.Close()
				return err
			}
			entries = append(entries, entry)
			cursor = entry.sequence
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO team_host_recovery_cursors(kind,position) VALUES('ports',?) ON CONFLICT(kind) DO UPDATE SET position=excluded.position`, cursor)
		return err
	})
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		// Host tombstones take precedence over still-pending port acquisitions.
		if entry.end == "" && entry.kind != "delivery" {
			var tombstone string
			err = receipts.db.QueryRowContext(ctx, `SELECT tombstone FROM team_host_intents WHERE intent_key=?`, entry.key).Scan(&tombstone)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				failures = append(failures, err)
				continue
			}
			if tombstone != "" {
				entry.end = tombstone
			}
		}
		switch entry.kind {
		case "session":
			if entry.end != "" {
				err = r.Sessions.Stop(ctx, entry.key)
			} else {
				var in teamhost.SessionRequest
				err = json.Unmarshal(entry.request, &in)
				if err == nil {
					_, err = r.Sessions.Launch(ctx, in)
				}
			}
		case "enrollment":
			if entry.end == "retire" {
				err = r.Enroller.Retire(ctx, entry.key)
			} else if entry.end != "" {
				err = r.Enroller.Release(ctx, entry.key)
			} else if entry.binding {
				err = r.Enroller.ReleaseBinding(ctx, entry.key)
			} else {
				var in teamhost.EnrollmentRequest
				err = json.Unmarshal(entry.request, &in)
				if err == nil {
					_, err = r.Enroller.Ensure(ctx, in)
				}
			}
		case "delivery":
			// The host owns dispatch/dead-letter decisions. Port recovery must never
			// bypass its queue, backoff or terminal delegation state.
			continue

		}
		if errors.Is(err, teams.ErrProvisionFailed) || errors.Is(err, teamhost.ErrInvalidRequest) || errors.Is(err, teamhost.ErrSessionGone) {
			err = errors.Join(err, receipts.failed(ctx, entry.kind, entry.key))
		}
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}
