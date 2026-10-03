package teamhost

import (
	"context"
	"database/sql"
	"errors"
	"github.com/hollis-labs/substrate/mesh"
	"time"

	"github.com/hollis-labs/substrate/mesh/teams"
)

// Ports distinguish permanent identity loss from temporary detachment. Ordinary
// errors, ErrSessionUnavailable, ErrDenied and ErrConflict are retryable.
var (
	ErrSessionGone        = errors.New("retained session ended")
	ErrInvalidRequest     = errors.New("invalid host port request")
	ErrSessionUnavailable = teams.ErrUnavailable
	// ErrPermanent is the explicit invalid-request class retained for port callers.
	ErrPermanent = ErrInvalidRequest
)

const maxRecoveryBackoff = time.Minute

func recoveryBackoff(attempts int) time.Duration {
	delay := time.Second
	for i := 1; i < attempts && delay < maxRecoveryBackoff; i++ {
		delay *= 2
	}
	if delay > maxRecoveryBackoff {
		delay = maxRecoveryBackoff
	}
	return delay
}

type recoveryEntry struct{ key, mode string }

// The cursor is durable and rotates past failures. Sequence follows insertion,
// not hash order; a reopened host resumes the next bounded page.
func (h *Host) recoveryPage(ctx context.Context, kind string, limit int) ([]recoveryEntry, error) {
	var entries []recoveryEntry
	err := h.tx(ctx, func(conn *sql.Conn) error {
		var cursor int64
		err := conn.QueryRowContext(ctx, `SELECT position FROM team_host_recovery_cursors WHERE kind=?`, kind).Scan(&cursor)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		query := `SELECT d.sequence,d.delivery_key,'' FROM team_host_deliveries d WHERE d.dispatched=0 AND d.dead=0 AND d.next_attempt_at<=? AND NOT EXISTS (SELECT 1 FROM team_host_deliveries earlier WHERE earlier.dispatched=0 AND earlier.dead=0 AND earlier.sequence<d.sequence AND json_extract(earlier.payload,'$.Recipient.actor')=json_extract(d.payload,'$.Recipient.actor') AND COALESCE(json_extract(earlier.payload,'$.Recipient.session_id'),'')=COALESCE(json_extract(d.payload,'$.Recipient.session_id'),'')) ORDER BY CASE WHEN sequence>? THEN 0 ELSE 1 END,sequence LIMIT ?`
		if kind == "intents" {
			query = `SELECT sequence,intent_key,tombstone FROM team_host_intents WHERE tombstone<>'' AND cleaned=0 AND dead=0 AND next_attempt_at<=? ORDER BY CASE WHEN sequence>? THEN 0 ELSE 1 END,sequence LIMIT ?`
		}
		rows, err := conn.QueryContext(ctx, query, h.Now().UnixNano(), cursor, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e recoveryEntry
			if err = rows.Scan(&cursor, &e.key, &e.mode); err != nil {
				return err
			}
			entries = append(entries, e)
		}
		if err = rows.Err(); err != nil {
			return err
		}
		if err = rows.Close(); err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO team_host_recovery_cursors(kind,position) VALUES(?,?) ON CONFLICT(kind) DO UPDATE SET position=excluded.position`, kind, cursor)
		return err
	})
	return entries, err
}
func (h *Host) finishAttempt(ctx context.Context, kind, key string, cause error) error {
	return h.tx(ctx, func(conn *sql.Conn) error {
		table, column, done := "team_host_deliveries", "delivery_key", "dispatched"
		if kind == "intents" {
			table, column, done = "team_host_intents", "intent_key", "cleaned"
		}
		permanent := errors.Is(cause, ErrInvalidRequest) || (kind == "deliveries" && errors.Is(cause, ErrSessionGone))
		var attempts int
		if err := conn.QueryRowContext(ctx, `SELECT attempts FROM `+table+` WHERE `+column+`=?`, key).Scan(&attempts); err != nil {
			return notFound(err)
		}
		next := int64(0)
		if cause != nil && !permanent {
			next = h.Now().Add(recoveryBackoff(attempts + 1)).UnixNano()
		}
		detail := ""
		if cause != nil {
			detail = cause.Error()
		}
		_, err := conn.ExecContext(ctx, `UPDATE `+table+` SET attempts=attempts+1,last_error=?,next_attempt_at=?,dead=CASE WHEN ? THEN 1 ELSE dead END,`+done+`=CASE WHEN ? THEN 1 ELSE `+done+` END WHERE `+column+`=?`, detail, next, permanent, cause == nil, key)
		if err != nil {
			return err
		}
		if kind == "deliveries" && permanent {
			// A dead delegation cannot remain working. A previously completed
			// reply/result is retained; only a non-terminal delegation fails.
			_, err = conn.ExecContext(ctx, `UPDATE team_host_delegations SET state=? WHERE delivery_key=? AND state=?`, mesh.TaskFailed, key, mesh.TaskWorking)
		}
		return err
	})
}

// DeadLetter is retained recovery evidence. Kind is "intents" or "deliveries".
type DeadLetter struct {
	Kind, Key, Error string
	Sequence         int64
	Attempts         int
}

func (h *Host) ListDeadLetters(ctx context.Context, limit int) ([]DeadLetter, error) {
	if limit < 1 {
		return nil, errors.New("dead letters: positive limit required")
	}
	rows, err := h.db.QueryContext(ctx, `SELECT kind,recovery_key,last_error,sequence,attempts FROM (SELECT 'intents' AS kind,intent_key AS recovery_key,last_error,sequence,attempts FROM team_host_intents WHERE dead=1 AND cleaned=0 UNION ALL SELECT 'deliveries',delivery_key,last_error,sequence,attempts FROM team_host_deliveries WHERE dead=1 AND dispatched=0) ORDER BY kind,sequence LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []DeadLetter
	for rows.Next() {
		var letter DeadLetter
		if err = rows.Scan(&letter.Kind, &letter.Key, &letter.Error, &letter.Sequence, &letter.Attempts); err != nil {
			return nil, err
		}
		result = append(result, letter)
	}
	return result, rows.Err()
}

// ReviveDeadLetter is an explicit operator repair seam. It keeps the original
// intent/delivery and its retained session, clears backoff and resets attempts.
// Terminal delegation outcomes remain terminal; revival cannot reopen a task.
func (h *Host) ReviveDeadLetter(ctx context.Context, kind, key string) error {
	table, column, done := "team_host_deliveries", "delivery_key", "dispatched"
	if kind == "intents" {
		table, column, done = "team_host_intents", "intent_key", "cleaned"
	} else if kind != "deliveries" {
		return errors.New("dead letters: invalid kind")
	}
	return h.tx(ctx, func(conn *sql.Conn) error {
		var completed bool
		if err := conn.QueryRowContext(ctx, `SELECT `+done+` FROM `+table+` WHERE `+column+`=?`, key).Scan(&completed); err != nil {
			return notFound(err)
		}
		if completed {
			return teams.ErrConflict
		}
		_, err := conn.ExecContext(ctx, `UPDATE `+table+` SET dead=0,next_attempt_at=0,attempts=0 WHERE `+column+`=?`, key)
		return err
	})
}
