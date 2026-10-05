//go:build !windows

package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/shimcodex"
)

// CodexCandidateSnapshot is a consistent read, not a delivery capability. The
// credential, filesystem custody and complete external commit fence are NOT
// established by this transaction. No projection/receipt table is activated.
type CodexCandidateSnapshot struct {
	Placement    SessionShimRow
	Protocol     shimcodex.State
	Route        *launchprofile.Route
	SessionState string
}

func (s *Store) ReadCodexCandidate(ctx context.Context, observed shimcodex.State) (CodexCandidateSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return CodexCandidateSnapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	return readCodexCandidateTx(ctx, tx, observed)
}

func readCodexCandidateTx(ctx context.Context, tx *sql.Tx, observed shimcodex.State) (CodexCandidateSnapshot, error) {
	var result CodexCandidateSnapshot
	if tx == nil || observed.Binding.Session == "" || observed.Revision == 0 {
		return result, ErrSessionShimConflict
	}
	placement, err := scanShim(tx.QueryRowContext(ctx, `SELECT `+shimColumns+` FROM session_shims WHERE session_id=?`, observed.Binding.Session))
	if err != nil {
		return result, err
	}
	if placement.ShimKey != observed.Binding.Operation || placement.RuntimeGeneration != observed.Binding.Generation || placement.JournalID != observed.Binding.Journal {
		return result, ErrSessionShimConflict
	}
	var protocol, revision, raw string
	err = tx.QueryRowContext(ctx, `SELECT protocol,revision,state_json FROM codex_shim_protocol WHERE session_id=? AND shim_key=? AND length(CAST(state_json AS BLOB))<=?`, placement.SessionID, placement.ShimKey, shimcodex.ProjectionBudget).Scan(&protocol, &revision, &raw)
	if err != nil {
		return result, err
	}
	if protocol != shimcodex.Version || revision != strconv.FormatUint(observed.Revision, 10) || json.Unmarshal([]byte(raw), &result.Protocol) != nil {
		return result, ErrSessionShimConflict
	}
	// Compare the complete frozen observation, including inbox/carry/input/callback
	// dispositions. A matching revision with different content is not admitted.
	frozen, err := json.Marshal(observed)
	if err != nil {
		return result, err
	}
	current, err := json.Marshal(result.Protocol)
	if err != nil || !bytes.Equal(current, frozen) {
		return result, ErrSessionShimConflict
	}
	err = tx.QueryRowContext(ctx, `SELECT state FROM sessions WHERE id=?`, placement.SessionID).Scan(&result.SessionState)
	if err != nil {
		return result, err
	}
	switch result.SessionState {
	case "completed", "failed", "killed", "orphaned":
		return result, ErrSessionShimConflict
	}
	result.Route, err = SessionRouteTx(ctx, tx, placement.SessionID)
	if err != nil {
		return result, err
	}
	result.Placement = placement
	return result, ctx.Err()
}

// stageCodexSourceTx is an internal executor composition. No app production
// issuer calls it. Its frozen identity never has legacy's alternate-body
// fallback. The caller owns rollback/commit, and a saved row is NOT a receipt.
func stageCodexSourceTx(ctx context.Context, tx *sql.Tx, env messaging.Envelope) (messaging.Envelope, error) {
	if tx == nil || env.ID == "" || env.From.Kind != messaging.KindSession || env.From.Authority != "local" || env.ThreadID != env.From.ID || env.Metadata["session_id"] != env.From.ID || env.Metadata["codex_source_id"] == "" || env.CreatedAt.IsZero() || env.Kind != messaging.MsgKindNotice || env.To != (messaging.Address{Kind: messaging.KindService, Authority: "local", ID: "turn-output"}) {
		return messaging.Envelope{}, fmt.Errorf("invalid frozen Codex stage")
	}
	if _, err := messaging.ParseURN(env.From.URN()); err != nil {
		return messaging.Envelope{}, err
	}
	if _, err := messaging.ParseURN(env.To.URN()); err != nil {
		return messaging.Envelope{}, err
	}
	saved, err := insertStagedTurnOutput(ctx, tx, env)
	if err != nil {
		return messaging.Envelope{}, err
	}
	var staged, payloadPresent int
	if err := tx.QueryRowContext(ctx, `SELECT routing_staged,payload IS NOT NULL FROM messages WHERE id=?`, env.ID).Scan(&staged, &payloadPresent); err != nil || staged != 1 || payloadPresent != 1 {
		return messaging.Envelope{}, fmt.Errorf("codex stage collides with unstaged or purged message")
	}
	a, _ := json.Marshal(env.Metadata)
	b, _ := json.Marshal(saved.Metadata)
	if !bytes.Equal(saved.Payload, env.Payload) || saved.ContentType != env.ContentType || saved.From != env.From || saved.To != env.To || saved.ThreadID != env.ThreadID || saved.Kind != env.Kind || !saved.CreatedAt.Equal(env.CreatedAt) || !bytes.Equal(a, b) {
		return messaging.Envelope{}, fmt.Errorf("conflicting frozen Codex source")
	}
	return saved, nil
}

// AcceptCodexDelivery intentionally cannot mint success. A pure candidate or
// fixture fence never supplies the missing complete external commit authority.
func (*Store) AcceptCodexDelivery(context.Context, shimcodex.Projection) (*VerifiedCodexDelivery, error) {
	return nil, ErrCodexDeliveryUnsupported
}
