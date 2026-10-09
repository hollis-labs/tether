//go:build !windows

package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strconv"

	"github.com/hollis-labs/tether/internal/shimcodex"
)

// CodexCustodyRefusal is a bounded diagnostic, never provider payload or an
// underlying database error. No value grants replacement or retirement rights.
type CodexCustodyRefusal string

const (
	CodexCustodyUnavailable        CodexCustodyRefusal = "custody_unavailable"
	CodexCustodySnapshotConflict   CodexCustodyRefusal = "snapshot_conflict"
	CodexCustodyCheckpointInvalid  CodexCustodyRefusal = "checkpoint_invalid"
	CodexCustodyObligationsPending CodexCustodyRefusal = "obligations_pending"
	CodexCustodyDeliveryUnverified CodexCustodyRefusal = "delivery_unverified"
	CodexCustodySnapshotChanged    CodexCustodyRefusal = "snapshot_changed"
)

// CodexCustodyObligations describes one frozen stored observation. False flags,
// including an empty Refusal, do NOT establish complete custody, external host
// identity, credential authority, or safe retirement/replacement. DeliveryVerified
// means only that the existing public delivery verifier accepted this snapshot;
// its opaque capability is deliberately not exposed here. Callers must not use
// this diagnostic as a lifecycle fence.
type CodexCustodyObligations struct {
	Revision         uint64
	SnapshotVerified bool
	Pending          bool
	Partial          bool
	Inbox            bool
	Active           bool
	Unknown          bool
	DeliveryVerified bool
	Refusal          CodexCustodyRefusal
}

// EvaluateCodexCustodyObligations makes bounded, read-only observations of
// canonical custody and the exact protocol revision. All unavailable, ambiguous
// and unsupported evidence fails closed. Only cancellation/deadline errors are
// returned; other failures become secret-free refusal codes. The supplied state
// remains private and is never copied into the result.
func (s *Store) EvaluateCodexCustodyObligations(ctx context.Context, observed shimcodex.State) (CodexCustodyObligations, error) {
	result := CodexCustodyObligations{Unknown: true, Refusal: CodexCustodyUnavailable}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if s == nil || s.db == nil || observed.Binding.Session == "" {
		return result, nil
	}
	before, err := s.readCodexObligationSnapshot(ctx, observed.Binding.Session)
	if err != nil {
		return result, ctx.Err()
	}
	result.Revision = before.protocol.Revision
	result.Refusal = CodexCustodySnapshotConflict
	frozen, err := json.Marshal(observed)
	if err != nil || !bytes.Equal(frozen, before.raw) {
		return result, nil
	}
	state := before.protocol
	row := before.placement
	if row.Runtime != "codex" || row.SessionID != state.Binding.Session || row.ShimKey != state.Binding.Operation || row.RuntimeGeneration != state.Binding.Generation || row.JournalID == "" || row.JournalID != state.Binding.Journal {
		return result, nil
	}
	result.Refusal = CodexCustodyCheckpointInvalid
	// Reuse the protocol's complete validator without allowing Open to bind an
	// intent, advance an epoch, commit, or create a missing checkpoint.
	if _, err = shimcodex.Open(ctx, codexObligationView{state}, state.Binding, state.Epoch, false, shimcodex.Limits{InboxItems: shimcodex.ProjectionSources, InboxBytes: shimcodex.ProjectionBudget}); err != nil {
		return result, ctx.Err()
	}
	result.SnapshotVerified = true
	result.Unknown = false
	result.Partial = len(state.Partial) != 0
	result.Inbox = len(state.Inbox) != 0
	result.Active = state.ActiveTurn != ""
	for _, op := range state.Operations {
		result.Unknown = result.Unknown || op.EffectUnknown
		switch op.Phase {
		case shimcodex.Intent:
			result.Pending = true
		case shimcodex.Attempted, shimcodex.Written:
			result.Pending, result.Unknown = true, true
		case shimcodex.Answered, shimcodex.NotSubmitted:
		default:
			result.Pending, result.Unknown = true, true
		}
	}
	for _, request := range state.ServerRequests {
		result.Pending = result.Pending || !request.Written
	}
	if p := state.Delivery; p != nil {
		result.Pending = result.Pending || len(p.OutstandingInputIDs) != 0 || len(p.OutstandingServerRequestSources) != 0
		result.Active = result.Active || p.ActiveTurnID != ""
		for _, turn := range p.Turns {
			result.Active = result.Active || turn.Phase == "open"
		}
		for _, source := range p.Sources {
			result.Unknown = result.Unknown || source.Disposition == "retained_unsupported"
		}
	}
	// Do not hold our read transaction while invoking the existing verifier:
	// stores with one connection must be able to perform its own read transaction.
	proof, err := s.LoadVerifiedCodexDelivery(ctx, state, state.ReplayHighWater)
	result.DeliveryVerified = err == nil && proof != nil
	if !result.DeliveryVerified {
		result.Unknown = true
	}
	result.Refusal = ""
	if result.Pending || result.Partial || result.Inbox || result.Active {
		result.Refusal = CodexCustodyObligationsPending
	} else if result.Unknown || !result.DeliveryVerified {
		result.Refusal = CodexCustodyDeliveryUnverified
	}
	// Refuse intervening canonical placement, session-state, or protocol changes.
	// This is still an observation, not an atomic fence for a later mutation.
	after, err := s.readCodexObligationSnapshot(ctx, observed.Binding.Session)
	if err != nil || before.placement != after.placement || before.sessionState != after.sessionState || !bytes.Equal(before.raw, after.raw) {
		result.SnapshotVerified, result.DeliveryVerified = false, false
		result.Unknown, result.Refusal = true, CodexCustodySnapshotChanged
	}
	return result, ctx.Err()
}

type codexObligationSnapshot struct {
	placement    SessionShimRow
	protocol     shimcodex.State
	raw          []byte
	sessionState string
}

func (s *Store) readCodexObligationSnapshot(ctx context.Context, session string) (codexObligationSnapshot, error) {
	var snapshot codexObligationSnapshot
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return snapshot, err
	}
	defer func() { _ = tx.Rollback() }()
	snapshot.placement, err = scanShim(tx.QueryRowContext(ctx, `SELECT `+shimColumns+` FROM session_shims WHERE session_id=?`, session))
	if err != nil {
		return snapshot, err
	}
	var protocol, revision string
	err = tx.QueryRowContext(ctx, `SELECT protocol,revision,state_json FROM codex_shim_protocol WHERE session_id=? AND shim_key=? AND length(CAST(state_json AS BLOB))<=?`, session, snapshot.placement.ShimKey, shimcodex.ProjectionBudget).Scan(&protocol, &revision, &snapshot.raw)
	if err != nil {
		return snapshot, err
	}
	if protocol != shimcodex.Version || json.Unmarshal(snapshot.raw, &snapshot.protocol) != nil || snapshot.protocol.Version != protocol || revision != strconv.FormatUint(snapshot.protocol.Revision, 10) {
		return snapshot, ErrSessionShimConflict
	}
	// The sole writers persist canonical JSON. Reject unknown fields, duplicate
	// keys, and other noncanonical encodings rather than erasing ambiguous data.
	canonical, err := json.Marshal(snapshot.protocol)
	if err != nil || !bytes.Equal(canonical, snapshot.raw) {
		return snapshot, ErrSessionShimConflict
	}
	err = tx.QueryRowContext(ctx, `SELECT state FROM sessions WHERE id=?`, session).Scan(&snapshot.sessionState)
	if err != nil {
		return snapshot, err
	}
	return snapshot, ctx.Err()
}

type codexObligationView struct{ state shimcodex.State }

func (v codexObligationView) Load(ctx context.Context) (shimcodex.State, error) {
	return v.state, ctx.Err()
}

func (codexObligationView) Commit(context.Context, uint64, shimcodex.State) error {
	return ErrCodexDeliveryUnsupported
}
