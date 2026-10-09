//go:build !windows

package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	messaging "github.com/hollis-labs/substrate/mesh/messaging"
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
	var replaced bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=?)`, observed.Binding.Session).Scan(&replaced); err != nil || replaced {
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

// CommitDelivery is the sole issuer: it reinterprets the exact frozen inbox,
// checks public acceptance against native content, then commits the projection
// and inbox drain in one CAS transaction. A pure candidate earns no receipt.
func (p *CodexProtocolStore) CommitDelivery(ctx context.Context, previous, next shimcodex.State) error {
	tx, err := p.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = readCodexCandidateTx(ctx, tx, previous); err != nil {
		return err
	}
	if next.Delivery == nil || next.Binding.Session != p.session || next.Binding.Operation != p.key || next.Revision != previous.Revision+1 || len(next.Inbox) != 0 {
		return ErrSessionShimConflict
	}
	expected, err := shimcodex.BuildDeliveryProjection(previous)
	if err != nil {
		return err
	}
	expected.ProtocolRevision = next.Revision
	expected.DeliveredHighWater = previous.Cursor
	// Only actual acceptance references may differ from the pure interpretation.
	if len(expected.Turns) != len(next.Delivery.Turns) {
		return ErrSessionShimConflict
	}
	for i := range expected.Turns {
		candidate := next.Delivery.Turns[i]
		if expected.Turns[i].Phase == "open" {
			if candidate.OutputAcceptanceID != "" || len(candidate.OutboxMessageIDs) != 0 {
				return ErrSessionShimConflict
			}
			continue
		}
		if err = p.store.verifyCodexTurnTx(ctx, tx, previous.Binding.Session, candidate); err != nil {
			return err
		}
		if expected.Turns[i].OutputAcceptanceID != "" && expected.Turns[i].OutputAcceptanceID != candidate.OutputAcceptanceID {
			return ErrSessionShimConflict
		}
		expected.Turns[i].OutputAcceptanceID = candidate.OutputAcceptanceID
		expected.Turns[i].OutboxMessageIDs = candidate.OutboxMessageIDs
	}
	a, _ := json.Marshal(expected)
	b, _ := json.Marshal(next.Delivery)
	if !bytes.Equal(a, b) {
		return ErrSessionShimConflict
	}
	canonical := previous
	canonical.Revision = next.Revision
	canonical.Inbox = nil
	canonical.Delivery = &expected
	a, _ = json.Marshal(canonical)
	b, _ = json.Marshal(next)
	if !bytes.Equal(a, b) || len(b) > p.maxBytes {
		return ErrSessionShimConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE codex_shim_protocol SET revision=?,state_json=? WHERE session_id=? AND shim_key=? AND revision=? AND NOT EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=?)`, strconv.FormatUint(next.Revision, 10), string(b), p.session, p.key, strconv.FormatUint(previous.Revision, 10), p.session)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return ErrSessionShimConflict
	}
	return tx.Commit()
}

func codexTurnKind(turn shimcodex.ProjectedTurn) string {
	switch turn.Phase {
	case "failed":
		return "failure"
	case "interrupted":
		return "terminal"
	default:
		return "final"
	}
}

func (s *Store) verifyCodexTurnTx(ctx context.Context, tx *sql.Tx, sessionID string, turn shimcodex.ProjectedTurn) error {
	if turn.OutputAcceptanceID == "" || len(turn.OutboxMessageIDs) > 1 || turn.CompletionSourceID == "" {
		return ErrCodexDeliveryUnsupported
	}
	text := shimcodex.ProjectedTurnText(turn)
	kind := codexTurnKind(turn)
	messageID := ""
	if len(turn.OutboxMessageIDs) == 1 {
		messageID = turn.OutboxMessageIDs[0]
	}
	if err := s.VerifyTurnOutputContent(ctx, tx, sessionID, turn.OutputAcceptanceID, messageID, turn.StableOutputTurnID, turn.CompletionSourceID, text, kind); err != nil {
		return err
	}
	output, found, err := publishedTurnOutput(ctx, tx, sessionID, turn.OutputAcceptanceID)
	if err != nil {
		return err
	}
	if !found || string(output.Kind) != kind || output.ProviderResultID != turn.CompletionSourceID || output.StopReason != turn.StopReason || output.Runtime != "codex" {
		return ErrSessionShimConflict
	}
	return nil
}

func (s *Store) LoadVerifiedCodexDelivery(ctx context.Context, state shimcodex.State, high string) (*VerifiedCodexDelivery, error) {
	if s == nil || s.db == nil {
		return nil, ErrCodexDeliveryUnsupported
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = readCodexCandidateTx(ctx, tx, state); err != nil {
		return nil, err
	}
	p := state.Delivery
	if p == nil || shimcodex.ValidateProjection(*p) != nil || p.Binding != state.Binding || p.DeliveredHighWater != state.Cursor || p.AcceptedSourceCursor != state.Cursor {
		return nil, ErrCodexDeliveryUnsupported
	}
	for _, source := range p.Sources {
		if source.Disposition == "retained_unsupported" {
			return nil, ErrCodexDeliveryUnsupported
		}
	}
	for _, turn := range p.Turns {
		if turn.Phase != "open" {
			if err = s.verifyCodexTurnTx(ctx, tx, state.Binding.Session, turn); err != nil {
				return nil, err
			}
		}
	}
	raw, _ := json.Marshal(state)
	projection, _ := json.Marshal(p)
	digest := sha256.Sum256(projection)
	proof := &VerifiedCodexDelivery{issuer: s, receiptID: hex.EncodeToString(digest[:]), observationDigest: sha256.Sum256(raw), acceptedCursor: state.Cursor, replayHighWater: high, deliveredCursor: p.DeliveredHighWater}
	if p.Terminal != nil && state.Exit != nil {
		proof.terminalIdentity = p.Terminal.SourceEventID
		proof.terminal = *state.Exit
	}
	if err = proof.Validate(s, state, high, state.Exit != nil); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return proof, nil
}
