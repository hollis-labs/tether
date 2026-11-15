package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store"
)

// The Codex shim's protocol state (codex_shim_protocol) is shimcodex.State JSON, the shim slice's
// (S9) type. The store keeps it as a row: it resolves the row only for the session's canonical
// placement, bounds what it reads, refuses a session that has been replaced (frozen old custody
// can never become a live runtime again), and writes only under the revision the caller read. The
// slice decodes, validates and compares the state, builds delivery projections and verifies turn
// outputs (S7's published outputs) inside the same transaction through the Tx variants here.

// ErrCodexProtocolMissing is returned when the session has no protocol row yet (shimcodex.ErrMissing
// in the old tree).
var ErrCodexProtocolMissing = errors.New("codex shim protocol state missing")

// CodexProtocolRow is one codex_shim_protocol row (the revision is the state's, as decimal text).
type CodexProtocolRow struct {
	SessionID, ShimKey, Protocol, Revision, StateJSON string
}

// shimBoundedLength is "column's byte length <= ?" on each dialect.
func shimBoundedLength(q gostore.Querier, column string) string {
	if q.Dialect().Name() == gostore.DriverPostgres {
		return `octet_length(` + column + `) <= ?`
	}
	return `length(CAST(` + column + ` AS BLOB)) <= ?`
}

// CodexSessionReplaced reports whether the session is the source of a replacement (its custody is
// frozen: no operational protocol load or write may proceed).
func CodexSessionReplaced(ctx context.Context, q gostore.Querier, sessionID string) (bool, error) {
	var replaced bool
	err := q.QueryRowContext(ctx, rebind(q, `SELECT EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id = ?)`), sessionID).Scan(&replaced)
	return replaced, err
}

// CodexProtocolTx reads the session's protocol row for shim key on q, at most maxBytes of state:
// ErrSessionShimConflict when the session is replaced, the key is not the canonical placement's,
// or the stored state is larger than maxBytes; ErrCodexProtocolMissing when there is no row.
func CodexProtocolTx(ctx context.Context, q gostore.Querier, sessionID, key string, maxBytes int) (CodexProtocolRow, error) {
	row := CodexProtocolRow{SessionID: sessionID, ShimKey: key}
	if sessionID == "" || key == "" || maxBytes <= 0 {
		return row, ErrSessionShimConflict
	}
	if replaced, err := CodexSessionReplaced(ctx, q, sessionID); err != nil {
		return row, err
	} else if replaced {
		return row, ErrSessionShimConflict
	}
	placement, err := SessionShimTx(ctx, q, sessionID)
	if err != nil {
		return row, err
	}
	if placement.ShimKey != key {
		return row, ErrSessionShimConflict
	}
	err = q.QueryRowContext(ctx, rebind(q, `SELECT protocol, revision, state_json FROM codex_shim_protocol
 WHERE session_id = ? AND shim_key = ? AND `+shimBoundedLength(q, "state_json")), sessionID, key, maxBytes).Scan(&row.Protocol, &row.Revision, &row.StateJSON)
	if errors.Is(err, sql.ErrNoRows) {
		var exists bool
		if e := q.QueryRowContext(ctx, rebind(q, `SELECT EXISTS(SELECT 1 FROM codex_shim_protocol WHERE session_id = ?)`), sessionID).Scan(&exists); e != nil {
			return row, e
		} else if !exists {
			return row, ErrCodexProtocolMissing
		}
		return row, ErrSessionShimConflict
	}
	return row, err
}

// CodexProtocol reads the session's protocol row in one snapshot (see CodexProtocolTx).
func (s *Store) CodexProtocol(ctx context.Context, sessionID, key string, maxBytes int) (CodexProtocolRow, error) {
	var row CodexProtocolRow
	err := s.c.ReadTx(ctx, func(ctx context.Context, q gostore.Querier) error {
		var err error
		row, err = CodexProtocolTx(ctx, q, sessionID, key, maxBytes)
		return err
	})
	return row, err
}

// CodexProtocolWrite is one protocol state write: Previous 0 inserts the first state; any other
// Previous replaces the stored state only while it still has that revision. Generation is the
// placement's runtime generation the state is bound to.
type CodexProtocolWrite struct {
	SessionID, ShimKey, Protocol string
	Generation                   uint64
	Previous, Next               uint64
	StateJSON                    string
	MaxBytes                     int
}

// CommitCodexProtocol writes a protocol state transition. The write applies only while the
// session's placement still has this key and generation and the session is not replaced; on
// Postgres the protocol row is locked first, so two transitions from the same revision cannot both
// apply. ErrSessionShimConflict when it does not apply.
func (s *Store) CommitCodexProtocol(ctx context.Context, w CodexProtocolWrite) error {
	if w.SessionID == "" || w.ShimKey == "" || w.Previous == ^uint64(0) || w.Next != w.Previous+1 || w.MaxBytes <= 0 || len(w.StateJSON) > w.MaxBytes {
		return ErrSessionShimConflict
	}
	return s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := shimLockSession(ctx, tx, w.SessionID); err != nil {
			return err
		}
		var res sql.Result
		var err error
		if w.Previous == 0 {
			res, err = tx.ExecContext(ctx, rebind(tx, `INSERT INTO codex_shim_protocol (session_id, shim_key, protocol, revision, state_json)
 SELECT ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM session_shims WHERE session_id = ? AND shim_key = ? AND runtime_generation = ?)
 AND NOT EXISTS (SELECT 1 FROM session_replacements WHERE source_session_id = ?)
 ON CONFLICT (session_id) DO NOTHING`), w.SessionID, w.ShimKey, w.Protocol, strconv.FormatUint(w.Next, 10), w.StateJSON,
				w.SessionID, w.ShimKey, strconv.FormatUint(w.Generation, 10), w.SessionID)
		} else {
			res, err = tx.ExecContext(ctx, rebind(tx, `UPDATE codex_shim_protocol SET revision = ?, state_json = ?
 WHERE session_id = ? AND shim_key = ? AND protocol = ? AND revision = ?
 AND EXISTS (SELECT 1 FROM session_shims WHERE session_id = ? AND shim_key = ? AND runtime_generation = ?)
 AND NOT EXISTS (SELECT 1 FROM session_replacements WHERE source_session_id = ?)`),
				strconv.FormatUint(w.Next, 10), w.StateJSON, w.SessionID, w.ShimKey, w.Protocol, strconv.FormatUint(w.Previous, 10),
				w.SessionID, w.ShimKey, strconv.FormatUint(w.Generation, 10), w.SessionID)
		}
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return ErrSessionShimConflict
		}
		return nil
	})
}

// CodexCandidate is one consistent read of a Codex session's custody: its placement, its protocol
// row (bounded), its state and its route JSON. It is a read, not a delivery capability.
type CodexCandidate struct {
	Placement    SessionShimRow
	Protocol     CodexProtocolRow
	SessionState string
	RouteJSON    string // "" when NULL
}

// CodexCandidateRead says how to read a candidate.
type CodexCandidateRead struct {
	MaxBytes int // bound on the protocol state read
	// RefuseReplaced refuses a replaced session (ErrSessionShimConflict), as the operational
	// delivery path does; the obligation and historical accounting readers leave it false.
	RefuseReplaced bool
	// ForUpdate locks the protocol row on Postgres: set it when reading inside the caller's class C
	// WriteTx before a revision compare-and-set (CommitCodexDeliveryTx), never in a read snapshot.
	ForUpdate bool
}

// CodexCandidateTx reads the candidate on q (a reader snapshot, or the caller's WriteTx).
func CodexCandidateTx(ctx context.Context, q gostore.Querier, sessionID string, opt CodexCandidateRead) (CodexCandidate, error) {
	var c CodexCandidate
	if sessionID == "" || opt.MaxBytes <= 0 {
		return c, ErrSessionShimConflict
	}
	if opt.RefuseReplaced {
		if replaced, err := CodexSessionReplaced(ctx, q, sessionID); err != nil {
			return c, err
		} else if replaced {
			return c, ErrSessionShimConflict
		}
	}
	placement, err := SessionShimTx(ctx, q, sessionID)
	if err != nil {
		return c, err
	}
	c.Placement = placement
	c.Protocol = CodexProtocolRow{SessionID: sessionID, ShimKey: placement.ShimKey}
	lock := ""
	if opt.ForUpdate {
		lock = repForUpdate(q)
	}
	err = q.QueryRowContext(ctx, rebind(q, `SELECT protocol, revision, state_json FROM codex_shim_protocol
 WHERE session_id = ? AND shim_key = ? AND `+shimBoundedLength(q, "state_json")+lock), sessionID, placement.ShimKey, opt.MaxBytes).
		Scan(&c.Protocol.Protocol, &c.Protocol.Revision, &c.Protocol.StateJSON)
	if err != nil {
		return c, err
	}
	var route sql.NullString
	if err := q.QueryRowContext(ctx, rebind(q, `SELECT state, route_json FROM sessions WHERE id = ?`), sessionID).Scan(&c.SessionState, &route); err != nil {
		return c, err
	}
	c.RouteJSON = route.String
	return c, ctx.Err()
}

// CodexCandidate reads the candidate in one read-only snapshot (ForUpdate is ignored).
func (s *Store) CodexCandidate(ctx context.Context, sessionID string, opt CodexCandidateRead) (CodexCandidate, error) {
	opt.ForUpdate = false
	var c CodexCandidate
	err := s.c.ReadTx(ctx, func(ctx context.Context, q gostore.Querier) error {
		var err error
		c, err = CodexCandidateTx(ctx, q, sessionID, opt)
		return err
	})
	return c, err
}

// CommitCodexDeliveryTx is the delivery commit's write, inside the caller's class C WriteTx after
// CodexCandidateTx (which locked the row) and the slice's projection and turn-output checks: it
// replaces the state only while it still has revision previous and the session is not replaced.
func CommitCodexDeliveryTx(ctx context.Context, tx store.Tx, sessionID, key string, previous, next uint64, stateJSON string, maxBytes int) error {
	if next != previous+1 || maxBytes <= 0 || len(stateJSON) > maxBytes {
		return ErrSessionShimConflict
	}
	res, err := tx.ExecContext(ctx, rebind(tx, `UPDATE codex_shim_protocol SET revision = ?, state_json = ?
 WHERE session_id = ? AND shim_key = ? AND revision = ? AND NOT EXISTS (SELECT 1 FROM session_replacements WHERE source_session_id = ?)`),
		strconv.FormatUint(next, 10), stateJSON, sessionID, key, strconv.FormatUint(previous, 10), sessionID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err == nil {
			err = ErrSessionShimConflict
		}
		return err
	}
	return nil
}

// CodexPlanProvider returns the provider id and brand the session's launch plan recorded (the
// inbox wire recovery requires the exact original association, without the catalog). A plan
// larger than maxBytes, or one that is not JSON, reads as empty strings. On Postgres the plan is
// read under the pg_input_is_valid guard after each \u0000 escape is replaced with U+FFFD.
func (s *Store) CodexPlanProvider(ctx context.Context, sessionID string, maxBytes int) (providerID, brand string, err error) {
	r := s.c.Reader()
	id, b := `json_extract(plan_json, '$.provider_id')`, `json_extract(plan_json, '$.provider_brand')`
	valid := `json_valid(plan_json)`
	if r.Dialect().Name() == gostore.DriverPostgres {
		const plan = `replace(plan_json, '\u0000', '�')`
		valid = `pg_input_is_valid(` + plan + `, 'jsonb')`
		id, b = `(`+plan+`)::jsonb->>'provider_id'`, `(`+plan+`)::jsonb->>'provider_brand'`
	}
	var pid, pbrand sql.NullString
	err = r.QueryRowContext(ctx, rebind(r, `SELECT CASE WHEN `+valid+` THEN `+id+` END, CASE WHEN `+valid+` THEN `+b+` END
 FROM launch_plans WHERE session_id = ? AND `+shimBoundedLength(r, "plan_json")), sessionID, maxBytes).Scan(&pid, &pbrand)
	return pid.String, pbrand.String, err
}
