package sessionstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store"
)

// ErrWorkspaceAllocationConflict refuses an unproven or changed allocation.
var ErrWorkspaceAllocationConflict = errors.New("workspace allocation unavailable")

// WorkspaceScratchAllocation records an earned allocation, not launch or deletion authority.
// Reserved rows survive failed and interrupted allocation.
type WorkspaceScratchAllocation struct {
	SessionID, OperationID, SessionDigest, PlanDigest string
	Root, Status                                      string
	PhysicalIdentity                                  ScratchPhysicalIdentity
}

// ScratchPhysicalIdentity binds both the accepted parent and the allocated child. These
// observations confer no authority independently of admission.
type ScratchPhysicalIdentity struct {
	Workspace string
	Scratch   string
}

// WorkspaceLaunchDecision is the session row, its exact stored plan JSON (unknown fields included)
// and its process start stamp, read together. It is never a new launch grant.
type WorkspaceLaunchDecision struct {
	Session  SessionRow
	PlanJSON string
	// PIDStartedAt is sessions.pid_started_at ("" for NULL). It is stored outside SessionRow but is
	// part of the canonical preparation predicate.
	PIDStartedAt string
}

// wsLockDecision takes the session's decision lock inside a class C WriteTx. The old store issued a
// no-op UPDATE of the session row to take SQLite's writer reservation before reading; here the
// WriteTx is already IMMEDIATE on SQLite (the one writer is held from BEGIN), and on Postgres the
// session row and its launch plan row are locked FOR UPDATE, so a concurrent state or plan change
// (and a second admission of the same session) waits until this transaction ends and then sees
// what it wrote. A session_shims writer is not blocked by these row locks; the shim placement port
// (S9) must lock the session row before inserting, as the old writer reservation implied.
func wsLockDecision(ctx context.Context, tx store.Tx, sessionID string) error {
	if tx.Dialect().Name() != gostore.DriverPostgres {
		return nil
	}
	for _, q := range []string{
		`SELECT id FROM sessions WHERE id = ? FOR UPDATE`,
		`SELECT session_id FROM launch_plans WHERE session_id = ? FOR UPDATE`,
	} {
		rows, err := tx.QueryContext(ctx, rebind(tx, q), sessionID)
		if err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}

// ReservePreparedWorkspaceScratch binds the exact post-preparation snapshot under the decision lock:
// a repeat of the same decision returns the stored reservation (existed true); any other decision
// for the session is ErrWorkspaceAllocationConflict.
func (s *Store) ReservePreparedWorkspaceScratch(ctx context.Context, decision WorkspaceLaunchDecision, operation string) (WorkspaceScratchAllocation, bool, error) {
	want, err := wsExpectation(decision, operation)
	if err != nil {
		return WorkspaceScratchAllocation{}, false, err
	}
	var out WorkspaceScratchAllocation
	var existed bool
	err = s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		out, existed = WorkspaceScratchAllocation{}, false
		if err := wsCheckDecision(ctx, tx, want); err != nil {
			return err
		}
		prior, err := wsReadAllocation(ctx, tx, want.SessionID)
		if err == nil {
			if !wsSameDecision(prior, want) {
				return ErrWorkspaceAllocationConflict
			}
			out, existed = prior, true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO workspace_scratch_allocations
 (session_id, operation_id, session_digest, plan_digest, root, status) VALUES (?, ?, ?, ?, ?, 'reserved')`),
			want.SessionID, want.OperationID, want.SessionDigest, want.PlanDigest, want.Root); err != nil {
			return err
		}
		out = want
		return nil
	})
	if err != nil {
		return WorkspaceScratchAllocation{}, false, err
	}
	return out, existed, nil
}

// AdmitWorkspaceScratch records identity only while the original decision and the caller's
// physical custody still hold. validate runs inside the write transaction and MUST NOT use this
// store or reenter lifecycle operations (it may run again if the transaction is retried). A failed
// check keeps the reserved row; it never adopts an unknown target.
func (s *Store) AdmitWorkspaceScratch(ctx context.Context, reserved WorkspaceScratchAllocation, physicalIdentity ScratchPhysicalIdentity, validate func(context.Context) error) (WorkspaceScratchAllocation, error) {
	if reserved.Status != "reserved" || reserved.PhysicalIdentity != (ScratchPhysicalIdentity{}) || physicalIdentity.Workspace == "" || physicalIdentity.Scratch == "" ||
		len(physicalIdentity.Workspace) > 256 || len(physicalIdentity.Scratch) > 256 || validate == nil {
		return WorkspaceScratchAllocation{}, ErrWorkspaceAllocationConflict
	}
	identityJSON, err := json.Marshal(physicalIdentity)
	if err != nil {
		return WorkspaceScratchAllocation{}, err
	}
	err = s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := wsCheckDecision(ctx, tx, reserved); err != nil {
			return err
		}
		prior, err := wsReadAllocation(ctx, tx, reserved.SessionID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(prior, reserved) {
			return ErrWorkspaceAllocationConflict
		}
		if err := validate(ctx); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, rebind(tx, `UPDATE workspace_scratch_allocations SET physical_identity = ?, status = 'admitted'
 WHERE session_id = ? AND operation_id = ? AND status = 'reserved' AND physical_identity = ''`),
			string(identityJSON), reserved.SessionID, reserved.OperationID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return ErrWorkspaceAllocationConflict
		}
		return nil
	})
	if err != nil {
		return WorkspaceScratchAllocation{}, err
	}
	reserved.Status, reserved.PhysicalIdentity = "admitted", physicalIdentity
	return reserved, nil
}

// wsExpectation is the allocation a decision earns for operation (a canonical UUID).
func wsExpectation(decision WorkspaceLaunchDecision, operation string) (WorkspaceScratchAllocation, error) {
	row := decision.Session
	op, err := uuid.Parse(operation)
	if err != nil || op.String() != operation || row.ID == "" || row.State != "created" || (row.PID != nil && *row.PID != 0) ||
		!json.Valid([]byte(decision.PlanJSON)) || decision.PlanJSON == "null" || !filepath.IsAbs(row.Workspace) || filepath.Clean(row.Workspace) != row.Workspace {
		return WorkspaceScratchAllocation{}, ErrWorkspaceAllocationConflict
	}
	rowDigest, err := wsSessionDigest(decision)
	if err != nil {
		return WorkspaceScratchAllocation{}, err
	}
	return WorkspaceScratchAllocation{SessionID: row.ID, OperationID: operation, SessionDigest: rowDigest, PlanDigest: wsPlanDigest(decision.PlanJSON),
		Root: filepath.Join(row.Workspace, ".tether-scratch-"+operation), Status: "reserved"}, nil
}

// wsSessionDigest digests the session row with its process start stamp. The row is this package's
// SessionRow, so a digest stored by the old binary (over its own row type) does not match: such a
// reservation is refused, never adopted.
func wsSessionDigest(d WorkspaceLaunchDecision) (string, error) {
	raw, err := json.Marshal(struct {
		Session      SessionRow
		PIDStartedAt string
	}{d.Session, d.PIDStartedAt})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// wsPlanDigest digests the exact stored plan text. Rehydrating and remarshalling would drop an
// unrecognized field and could mistake a changed plan for the accepted one.
func wsPlanDigest(plan string) string {
	sum := sha256.Sum256([]byte(plan))
	return hex.EncodeToString(sum[:])
}

// wsCheckDecision takes the decision lock and checks that the stored decision still is the one want
// was earned from, the session is still created with no process, and no shim holds it.
func wsCheckDecision(ctx context.Context, tx store.Tx, want WorkspaceScratchAllocation) error {
	if err := wsLockDecision(ctx, tx, want.SessionID); err != nil {
		return err
	}
	current, err := wsReadDecision(ctx, tx, want.SessionID)
	if err != nil {
		return err
	}
	if wsPlanDigest(current.PlanJSON) != want.PlanDigest {
		return ErrWorkspaceAllocationConflict
	}
	row := current.Session
	rowDigest, err := wsSessionDigest(current)
	if err != nil || rowDigest != want.SessionDigest || row.State != "created" || (row.PID != nil && *row.PID != 0) ||
		want.Root != filepath.Join(row.Workspace, ".tether-scratch-"+want.OperationID) {
		return ErrWorkspaceAllocationConflict
	}
	var shims int64
	if err := tx.QueryRowContext(ctx, rebind(tx, `SELECT COUNT(*) FROM session_shims WHERE session_id = ?`), want.SessionID).Scan(&shims); err != nil {
		return err
	}
	if shims > 0 {
		return ErrWorkspaceAllocationConflict
	}
	return nil
}

// WorkspaceLaunchDecision reads the session row and plan in one snapshot.
func (s *Store) WorkspaceLaunchDecision(ctx context.Context, id string) (WorkspaceLaunchDecision, error) {
	var d WorkspaceLaunchDecision
	err := s.c.ReadTx(ctx, func(ctx context.Context, q gostore.Querier) error {
		var err error
		d, err = wsReadDecision(ctx, q, id)
		return err
	})
	return d, err
}

// WorkspaceScratchAllocation reads the retained allocation facts; it cannot refresh a decision, adopt
// a resource or issue deletion eligibility.
func (s *Store) WorkspaceScratchAllocation(ctx context.Context, id string) (WorkspaceScratchAllocation, error) {
	return wsReadAllocation(ctx, s.c.Reader(), id)
}

// ValidateWorkspaceScratchStart recognizes only the manager's explicit created -> launching
// transition (zero pid and its updated_at). Every other session field, the exact prepared JSON and
// the allocation receipt must be unchanged. It holds no transaction over provider entry and grants
// no new launch rights.
func (s *Store) ValidateWorkspaceScratchStart(ctx context.Context, accepted WorkspaceLaunchDecision, allocation WorkspaceScratchAllocation) error {
	return s.c.ReadTx(ctx, func(ctx context.Context, q gostore.Querier) error {
		current, err := wsReadDecision(ctx, q, allocation.SessionID)
		if err != nil {
			return err
		}
		expected := accepted.Session
		expected.State = "launching"
		zero := int64(0)
		expected.PID, expected.ExitCode = &zero, nil
		expected.UpdatedAt = current.Session.UpdatedAt
		if _, err := time.Parse(time.RFC3339, expected.UpdatedAt); err != nil || current.PlanJSON != accepted.PlanJSON ||
			current.PIDStartedAt != accepted.PIDStartedAt || !reflect.DeepEqual(current.Session, expected) {
			return ErrWorkspaceAllocationConflict
		}
		retained, err := wsReadAllocation(ctx, q, allocation.SessionID)
		if err != nil {
			return err
		}
		if allocation.Status != "admitted" || !reflect.DeepEqual(retained, allocation) {
			return ErrWorkspaceAllocationConflict
		}
		return nil
	})
}

func wsReadDecision(ctx context.Context, q gostore.Querier, id string) (WorkspaceLaunchDecision, error) {
	var d WorkspaceLaunchDecision
	var started sql.NullString
	row, err := scanSession(func(dest ...any) error {
		return q.QueryRowContext(ctx, rebind(q, `SELECT `+sessionColumns+`, pid_started_at FROM sessions WHERE id = ?`), id).Scan(append(dest, &started)...)
	})
	if err != nil {
		return WorkspaceLaunchDecision{}, err
	}
	d.Session, d.PIDStartedAt = row, started.String
	err = q.QueryRowContext(ctx, rebind(q, `SELECT plan_json FROM launch_plans WHERE session_id = ?`), id).Scan(&d.PlanJSON)
	return d, err
}

func wsSameDecision(a, b WorkspaceScratchAllocation) bool {
	return a.SessionID == b.SessionID && a.OperationID == b.OperationID && a.SessionDigest == b.SessionDigest &&
		a.PlanDigest == b.PlanDigest && a.Root == b.Root
}

func wsReadAllocation(ctx context.Context, q gostore.Querier, id string) (WorkspaceScratchAllocation, error) {
	var a WorkspaceScratchAllocation
	var physical string
	err := q.QueryRowContext(ctx, rebind(q, `SELECT session_id, operation_id, session_digest, plan_digest, root, physical_identity, status
 FROM workspace_scratch_allocations WHERE session_id = ?`), id).Scan(&a.SessionID, &a.OperationID, &a.SessionDigest, &a.PlanDigest, &a.Root, &physical, &a.Status)
	if err != nil {
		return WorkspaceScratchAllocation{}, fmt.Errorf("read workspace allocation: %w", err)
	}
	if physical != "" {
		if err := json.Unmarshal([]byte(physical), &a.PhysicalIdentity); err != nil {
			return WorkspaceScratchAllocation{}, ErrWorkspaceAllocationConflict
		}
	}
	return a, nil
}

// WorkspacePreparationStamp holds only the values this admitted preparation produced. It cannot
// change the provider, roots, caller environment or authority.
type WorkspacePreparationStamp struct {
	RefAttribution, NativeStateRoot string
}

// WorkspacePreparationResult is the recorded decision. RefAttributionError is kept for the old
// API's shape: the attribution stamp is written in the same transaction as the plan, so a failed
// stamp now fails the whole preparation (nothing written) rather than being reported here.
type WorkspacePreparationResult struct {
	Decision            WorkspaceLaunchDecision
	RefAttributionError error
}

// RecordWorkspacePreparation checks the original exact row and plan under the decision lock, then
// records the two computed preparation stamps (ref_attribution and the plan's native_state_root).
// nil validates an unchanged ACP preparation. Any intervening change refuses, without refresh. An
// earned retry (an admitted allocation for the same decision) keeps its original snapshot.
func (s *Store) RecordWorkspacePreparation(ctx context.Context, before WorkspaceLaunchDecision, stamp *WorkspacePreparationStamp) (WorkspacePreparationResult, error) {
	want, err := wsExpectation(before, uuid.NewString())
	if err != nil {
		return WorkspacePreparationResult{}, err
	}
	if stamp != nil && (stamp.RefAttribution == "" || !filepath.IsAbs(stamp.NativeStateRoot) || filepath.Clean(stamp.NativeStateRoot) != stamp.NativeStateRoot) {
		// The old store checked this only on a first preparation; a retry compared the stamp against
		// the snapshot, which an invalid stamp cannot match either.
		return WorkspacePreparationResult{}, ErrWorkspaceAllocationConflict
	}
	var result WorkspacePreparationResult
	err = s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		result = WorkspacePreparationResult{}
		if err := wsCheckDecision(ctx, tx, want); err != nil {
			return err
		}
		prior, err := wsReadAllocation(ctx, tx, before.Session.ID)
		if err == nil {
			expected, err := wsExpectation(before, prior.OperationID)
			if err != nil || prior.Status != "admitted" || !wsSameDecision(prior, expected) {
				return ErrWorkspaceAllocationConflict
			}
			if stamp != nil {
				root, err := wsTopLevelText(before.PlanJSON, "native_state_root")
				if err != nil || before.Session.RefAttribution == "" || before.Session.RefAttribution != stamp.RefAttribution || root != stamp.NativeStateRoot {
					return ErrWorkspaceAllocationConflict
				}
			}
			result.Decision = before
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		after := before
		if stamp != nil {
			if after.PlanJSON, err = wsSetTopLevelString(before.PlanJSON, "native_state_root", stamp.NativeStateRoot); err != nil {
				return err
			}
			updatedAt := s.secondStamp()
			res, err := tx.ExecContext(ctx, rebind(tx, `UPDATE sessions SET ref_attribution = ?, updated_at = ? WHERE id = ?`), stamp.RefAttribution, updatedAt, before.Session.ID)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil || n != 1 {
				return ErrSessionNotFound
			}
			after.Session.RefAttribution, after.Session.UpdatedAt = stamp.RefAttribution, updatedAt
			res, err = tx.ExecContext(ctx, rebind(tx, `UPDATE launch_plans SET plan_json = ? WHERE session_id = ?`), after.PlanJSON, before.Session.ID)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil || n != 1 {
				return ErrSessionNotFound
			}
		}
		actual, err := wsReadDecision(ctx, tx, before.Session.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(actual, after) {
			return ErrWorkspaceAllocationConflict
		}
		result.Decision = after
		return nil
	})
	if err != nil {
		return WorkspacePreparationResult{}, err
	}
	return result, nil
}

// BeginWorkspaceScratchPlacement performs the created -> launching transition against the exact
// admitted snapshot, under the decision lock. It returns only what this transaction wrote, never a
// refreshed decision.
func (s *Store) BeginWorkspaceScratchPlacement(ctx context.Context, accepted WorkspaceLaunchDecision, allocation WorkspaceScratchAllocation) (WorkspaceLaunchDecision, error) {
	want, err := wsExpectation(accepted, allocation.OperationID)
	if err != nil || !wsSameDecision(want, allocation) {
		return WorkspaceLaunchDecision{}, ErrWorkspaceAllocationConflict
	}
	var after WorkspaceLaunchDecision
	err = s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := wsLockDecision(ctx, tx, accepted.Session.ID); err != nil {
			return err
		}
		if err := wsValidateExact(ctx, tx, accepted, allocation); err != nil {
			return err
		}
		after = accepted
		zero := int64(0)
		after.Session.State, after.Session.PID, after.Session.ExitCode = "launching", &zero, nil
		after.Session.UpdatedAt = s.secondStamp()
		if _, err := tx.ExecContext(ctx, rebind(tx, `UPDATE sessions SET state = 'launching', pid = 0, exit_code = NULL, updated_at = ? WHERE id = ?`),
			after.Session.UpdatedAt, accepted.Session.ID); err != nil {
			return err
		}
		return wsValidateExact(ctx, tx, after, allocation)
	})
	if err != nil {
		return WorkspaceLaunchDecision{}, err
	}
	return after, nil
}

// ValidateWorkspaceScratchPlacement checks the exact transition returned above immediately before
// placement, in one snapshot. No transaction spans a provider callback.
func (s *Store) ValidateWorkspaceScratchPlacement(ctx context.Context, accepted WorkspaceLaunchDecision, allocation WorkspaceScratchAllocation) error {
	return s.c.ReadTx(ctx, func(ctx context.Context, q gostore.Querier) error {
		return wsValidateExact(ctx, q, accepted, allocation)
	})
}

func wsValidateExact(ctx context.Context, q gostore.Querier, accepted WorkspaceLaunchDecision, allocation WorkspaceScratchAllocation) error {
	if accepted.Session.ID != allocation.SessionID || allocation.Status != "admitted" {
		return ErrWorkspaceAllocationConflict
	}
	current, err := wsReadDecision(ctx, q, allocation.SessionID)
	if err != nil {
		return err
	}
	retained, err := wsReadAllocation(ctx, q, allocation.SessionID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, accepted) || !reflect.DeepEqual(retained, allocation) {
		return ErrWorkspaceAllocationConflict
	}
	return nil
}

// wsTopLevelText is SQLite's COALESCE(json_extract(plan, '$.'+key), ”) for a top-level key: a
// string's value, "" for null or a missing key, and the compact JSON text of any other value
// (SQLite returns a number or boolean as its SQL value, which compares as that text).
func wsTopLevelText(plan, key string) (string, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(plan), &top); err != nil {
		return "", err
	}
	raw, ok := top[key]
	if !ok || string(raw) == "null" {
		return "", nil
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str, nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return "", err
	}
	switch buf.String() {
	case "true":
		return "1", nil
	case "false":
		return "0", nil
	}
	return buf.String(), nil
}

// wsSetTopLevelString is SQLite's json_set(plan, '$.'+key, value) for a top-level object key and a
// text value, done in Go so it runs the same on both dialects: the result is the plan minified (as
// json_set returns it), with key's value replaced in place, or key appended last when absent, so
// every other member keeps its bytes and order. The value is escaped as SQLite escapes text.
func wsSetTopLevelString(plan, key, value string) (string, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(plan)); err != nil {
		return "", err
	}
	src := compact.Bytes()
	dec := json.NewDecoder(bytes.NewReader(src))
	tok, err := dec.Token()
	if err != nil {
		return "", err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", fmt.Errorf("plan is not a JSON object")
	}
	encoded := wsQuote(value)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		name, _ := tok.(string)
		start := dec.InputOffset() + 1 // past the ':' (compact JSON has no space)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return "", err
		}
		end := dec.InputOffset()
		if name == key {
			return string(src[:start]) + encoded + string(src[end:]), nil
		}
	}
	if _, err := dec.Token(); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	closing := bytes.LastIndexByte(src, '}')
	sep := ","
	if strings.TrimSpace(string(src[1:closing])) == "" {
		sep = ""
	}
	return string(src[:closing]) + sep + wsQuote(key) + ":" + encoded + string(src[closing:]), nil
}

// wsQuote writes s as a JSON string the way SQLite's json functions do: '"' and '\' escaped, the
// control characters \b \f \n \r \t in short form and the others as \u00xx; everything else
// (including U+2028, U+2029 and non-ASCII) as its bytes.
func wsQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); {
		_, size := utf8.DecodeRuneInString(s[i:])
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\f':
			b.WriteString(`\f`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20:
			fmt.Fprintf(&b, `\u%04x`, c)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	b.WriteByte('"')
	return b.String()
}
