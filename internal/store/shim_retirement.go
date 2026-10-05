package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/hollis-labs/tether/internal/shimretire"
)

const maxRetirementRecord = 262144

var ErrShimRetirementConflict = errors.New("shim retirement revision conflict")

// ShimRetirementStore is the sole durable view used by retirement policy. The
// referenced migration belongs to the session schema owner. These records never
// contain a launch descriptor, provider env, journal output or bridge carry.
type ShimRetirementStore struct{ Store *Store }

func (s ShimRetirementStore) Load(ctx context.Context, operation string) (shimretire.Receipt, error) {
	var r shimretire.Receipt
	if s.Store == nil || operation == "" {
		return r, ErrShimRetirementConflict
	}
	var raw, revision, digest, session, phase string
	var retired sql.NullString
	err := s.Store.db.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(record_json AS BLOB))<=262144 THEN record_json ELSE NULL END,revision,request_digest,session_id,phase,retired_at FROM session_shim_retirements WHERE operation_id=?`, operation).Scan(&raw, &revision, &digest, &session, &phase, &retired)
	if errors.Is(err, sql.ErrNoRows) {
		return r, shimretire.ErrNotFound
	}
	if err != nil {
		return r, errors.New("retirement receipt unavailable")
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r) != nil || r.OperationID != operation || r.Revision != revision || r.RequestDigest != digest || r.Request.Placement.Session != session || string(r.Phase) != phase || !retirementTimeBound(r.RetiredAt, retired) || shimretire.ValidateReceipt(r) != nil {
		return shimretire.Receipt{}, ErrShimRetirementConflict
	}
	return r.Clone(), nil
}

func phaseRank(p shimretire.Phase) int {
	switch p {
	case shimretire.IntentRecorded:
		return 1
	case shimretire.RetirementCommitted:
		return 2
	case shimretire.DescriptorCleanupComplete:
		return 3
	case shimretire.StateReconciled:
		return 4
	}
	return 0
}

func (s ShimRetirementStore) Record(ctx context.Context, r shimretire.Receipt, expected string) (string, error) {
	if s.Store == nil || r.Revision != expected || shimretire.ValidateReceipt(r) != nil || s.durable(ctx) != nil {
		return "", ErrShimRetirementConflict
	}
	prior, err := s.Load(ctx, r.OperationID)
	if errors.Is(err, shimretire.ErrNotFound) {
		if expected != "" || r.Phase != shimretire.IntentRecorded {
			return "", ErrShimRetirementConflict
		}
	} else if err != nil {
		return "", err
	} else {
		if prior.Revision != expected || prior.RequestDigest != r.RequestDigest || prior.Request != r.Request || phaseRank(r.Phase) < phaseRank(prior.Phase) || phaseRank(r.Phase) > phaseRank(prior.Phase)+1 {
			return "", ErrShimRetirementConflict
		}
		if !prior.RetiredAt.IsZero() && !prior.RetiredAt.Equal(r.RetiredAt) {
			return "", ErrShimRetirementConflict
		}
		expectedSnapshot := prior.Snapshot
		expectedSnapshot.Retired = r.Snapshot.Retired
		if expectedSnapshot != r.Snapshot || !slices.Equal(prior.Inventory, r.Inventory) {
			return "", ErrShimRetirementConflict
		}
		for _, o := range prior.Obligations {
			found := false
			for _, next := range r.Obligations {
				if o == next {
					found = true
				}
			}
			if !found {
				return "", ErrShimRetirementConflict
			}
		}
	}
	var nonce [32]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return "", errors.New("retirement revision unavailable")
	}
	r.Revision = hex.EncodeToString(nonce[:])
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > maxRetirementRecord {
		return "", ErrShimRetirementConflict
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var retired any
	if !r.RetiredAt.IsZero() {
		retired = r.RetiredAt.UTC().Format(time.RFC3339Nano)
	}
	var result sql.Result
	if expected == "" {
		result, err = s.Store.db.ExecContext(ctx, `INSERT INTO session_shim_retirements(operation_id,session_id,request_digest,revision,phase,record_json,retired_at,created_at,updated_at) SELECT ?,?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM sessions WHERE id=?) ON CONFLICT(operation_id) DO NOTHING`, r.OperationID, r.Request.Placement.Session, r.RequestDigest, r.Revision, string(r.Phase), string(raw), retired, now, now, r.Request.Placement.Session)
	} else {
		result, err = s.Store.db.ExecContext(ctx, `UPDATE session_shim_retirements SET revision=?,phase=?,record_json=?,retired_at=?,updated_at=? WHERE operation_id=? AND revision=? AND request_digest=? AND session_id=?`, r.Revision, string(r.Phase), string(raw), retired, now, r.OperationID, expected, r.RequestDigest, r.Request.Placement.Session)
	}
	if err != nil {
		return "", errors.New("retirement receipt commit uncertain")
	}
	n, err := result.RowsAffected()
	if err != nil {
		return "", errors.New("retirement receipt commit uncertain")
	}
	if n != 1 {
		return "", ErrShimRetirementConflict
	}
	return r.Revision, nil
}

// ReconcileRetirement revokes authority and records orphaned state only after a
// bound durable cleanup receipt. It never invents a provider exit/ended_at. The
// caller still holds complete lifecycle/controller/placement exclusion.
func (s ShimRetirementStore) ReconcileRetirement(ctx context.Context, placement shimretire.Placement, operation string) (SessionStateChange, error) {
	change := SessionStateChange{SessionID: placement.Session, To: "orphaned", Reason: "explicit_retirement"}
	if s.Store == nil || s.durable(ctx) != nil {
		return change, ErrShimRetirementConflict
	}
	tx, err := s.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return change, errors.New("retirement reconciliation unavailable")
	}
	defer func() { _ = tx.Rollback() }()
	var raw, revision, digest, session, phase string
	var retired sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(record_json AS BLOB))<=262144 THEN record_json ELSE NULL END,revision,request_digest,session_id,phase,retired_at FROM session_shim_retirements WHERE operation_id=?`, operation).Scan(&raw, &revision, &digest, &session, &phase, &retired); err != nil || len(raw) > maxRetirementRecord {
		return change, ErrShimRetirementConflict
	}
	var receipt shimretire.Receipt
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&receipt) != nil || shimretire.ValidateReceipt(receipt) != nil || receipt.Request.Placement != placement || receipt.OperationID != operation || receipt.Revision != revision || receipt.RequestDigest != digest || session != placement.Session || string(receipt.Phase) != phase || !retirementTimeBound(receipt.RetiredAt, retired) || (receipt.Phase != shimretire.DescriptorCleanupComplete && receipt.Phase != shimretire.StateReconciled) {
		return change, ErrShimRetirementConflict
	}
	var key, generation, backend, unit, journal string
	if err = tx.QueryRowContext(ctx, `SELECT shim_key,runtime_generation,host_backend,unit_name,journal_id FROM session_shims WHERE session_id=?`, placement.Session).Scan(&key, &generation, &backend, &unit, &journal); err != nil || key != placement.OperationKey || generation != strconv.FormatUint(placement.Generation, 10) || backend != receipt.Snapshot.Backend || unit != receipt.Snapshot.Unit.Name || journal != receipt.Snapshot.Journal.ID {
		return change, ErrShimRetirementConflict
	}
	if err = tx.QueryRowContext(ctx, `SELECT state,COALESCE(logical_agent_id,'') FROM sessions WHERE id=?`, placement.Session).Scan(&change.From, &change.LogicalAgentID); err != nil {
		return change, ErrShimRetirementConflict
	}
	switch change.From {
	case "launching", "running", "detached", "orphaned":
	default:
		return change, ErrShimRetirementConflict
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE sessions SET state='orphaned',pid=NULL,pid_started_at=NULL,updated_at=? WHERE id=?`, []any{now, placement.Session}},
		{`UPDATE principals SET revoked_at=COALESCE(revoked_at,?) WHERE kind='session' AND session_id=?`, []any{now, placement.Session}},
		{`UPDATE runtime_bindings SET revoked_at=?,updated_at=? WHERE session_id=? AND revoked_at IS NULL`, []any{now, now, placement.Session}},
	} {
		if _, err = tx.ExecContext(ctx, statement.sql, statement.args...); err != nil {
			return change, errors.New("retirement reconciliation unavailable")
		}
	}
	if err = tx.Commit(); err != nil {
		return change, errors.New("retirement reconciliation outcome uncertain")
	}
	return change, nil
}

func retirementTimeBound(at time.Time, saved sql.NullString) bool {
	if at.IsZero() {
		return !saved.Valid
	}
	if !saved.Valid {
		return false
	}
	parsed, err := time.Parse(time.RFC3339Nano, saved.String)
	return err == nil && at.Equal(parsed)
}

// ShimRetentionStore keeps nonsecret cursor/audit outside deletable payloads.
// The scope inventory/proof adapter remains a separate trusted capability.
type ShimRetentionStore struct{ Store *Store }

func (s ShimRetentionStore) LoadCursor(ctx context.Context, id string) (shimretire.SweepCursor, error) {
	var c shimretire.SweepCursor
	if s.Store == nil {
		return c, ErrShimRetirementConflict
	}
	var raw, digest, revision string
	err := s.Store.db.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(record_json AS BLOB))<=262144 THEN record_json ELSE NULL END,request_digest,revision FROM session_shim_retention_cursors WHERE cursor_id=?`, id).Scan(&raw, &digest, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return c, shimretire.ErrNotFound
	}
	if err != nil {
		return c, errors.New("retention cursor unavailable")
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&c) != nil || c.ID != id || c.Revision != revision || c.RequestDigest != digest || shimretire.ValidateSweepCursor(c) != nil {
		return shimretire.SweepCursor{}, ErrShimRetirementConflict
	}
	return c.Clone(), nil
}
func (s ShimRetentionStore) RecordCursor(ctx context.Context, c shimretire.SweepCursor, expected string) (string, error) {
	if s.Store == nil || c.Revision != expected || shimretire.ValidateSweepCursor(c) != nil || ShimRetirementStore(s).durable(ctx) != nil {
		return "", ErrShimRetirementConflict
	}
	prior, err := s.LoadCursor(ctx, c.ID)
	if errors.Is(err, shimretire.ErrNotFound) {
		if expected != "" || c.Phase != shimretire.CursorReady || c.LastKey != "" || c.Pending != nil {
			return "", ErrShimRetirementConflict
		}
	} else if err != nil {
		return "", err
	} else if !validCursorTransition(prior, c) || prior.Revision != expected {
		return "", ErrShimRetirementConflict
	}
	var nonce [32]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return "", errors.New("retention cursor revision unavailable")
	}
	c.Revision = hex.EncodeToString(nonce[:])
	raw, err := json.Marshal(c)
	if err != nil || len(raw) > maxRetirementRecord {
		return "", ErrShimRetirementConflict
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var result sql.Result
	if expected == "" {
		result, err = s.Store.db.ExecContext(ctx, `INSERT INTO session_shim_retention_cursors(cursor_id,request_digest,revision,record_json,created_at,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(cursor_id) DO NOTHING`, c.ID, c.RequestDigest, c.Revision, string(raw), now, now)
	} else {
		result, err = s.Store.db.ExecContext(ctx, `UPDATE session_shim_retention_cursors SET revision=?,record_json=?,updated_at=? WHERE cursor_id=? AND revision=? AND request_digest=?`, c.Revision, string(raw), now, c.ID, expected, c.RequestDigest)
	}
	if err != nil {
		return "", errors.New("retention cursor commit uncertain")
	}
	n, err := result.RowsAffected()
	if err != nil {
		return "", errors.New("retention cursor commit uncertain")
	}
	if n != 1 {
		return "", ErrShimRetirementConflict
	}
	return c.Revision, nil
}
func validCursorTransition(old, next shimretire.SweepCursor) bool {
	if old.RequestDigest != next.RequestDigest || old.InventoryRevision != next.InventoryRevision || next.LastKey < old.LastKey || len(next.Retained) < len(old.Retained) || !slices.Equal(old.Retained, next.Retained[:len(old.Retained)]) {
		return false
	}
	switch old.Phase {
	case shimretire.CursorReady:
		return (next.Phase == shimretire.CursorReady && next.Pending == nil) || (next.Phase == shimretire.CursorIntent && next.LastKey == old.LastKey) || (next.Phase == shimretire.CursorComplete && next.LastKey == old.LastKey)
	case shimretire.CursorIntent:
		return next.LastKey == old.LastKey && next.Pending != nil && old.Pending != nil && *next.Pending == *old.Pending && (next.Phase == shimretire.CursorIntent || next.Phase == shimretire.CursorRemoved)
	case shimretire.CursorRemoved:
		return next.Phase == shimretire.CursorReady && next.Pending == nil && old.Pending != nil && next.LastKey == old.Pending.SortKey
	case shimretire.CursorComplete:
		return next.Phase == shimretire.CursorComplete && next.LastKey == old.LastKey
	}
	return false
}

// Refuse weakened connection durability rather than claiming a durable audit
// from an application configured with synchronous=OFF/NORMAL. The connection
// configuration remains owned by Store; this adapter never changes PRAGMAs.
func (s ShimRetirementStore) durable(ctx context.Context) error {
	var sync int
	if s.Store == nil || s.Store.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync) != nil || (sync != 2 && sync != 3) {
		return errors.New("durable retirement store unavailable")
	}
	return nil
}
