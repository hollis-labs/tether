//go:build !windows

package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/tether/internal/shimcodex"
)

// CodexProtocolStore resolves only a canonical existing shim row. Payloads can
// contain provider output; callers never expose this record as shim metadata.
type CodexProtocolStore struct {
	store        *Store
	session, key string
	maxBytes     int
}

func (s *Store) CodexProtocolStore(ctx context.Context, session, key string, maxBytes int) (*CodexProtocolStore, error) {
	if maxBytes <= 0 || session == "" || key == "" {
		return nil, ErrSessionShimConflict
	}
	if err := s.currentCodexExecution(ctx, session); err != nil {
		return nil, err
	}
	row, err := s.SessionShim(ctx, session)
	if err != nil {
		return nil, err
	}
	if row.ShimKey != key {
		return nil, ErrSessionShimConflict
	}
	return &CodexProtocolStore{store: s, session: session, key: key, maxBytes: maxBytes}, nil
}

func (p *CodexProtocolStore) validateBinding(ctx context.Context, state shimcodex.State) error {
	row, err := p.store.SessionShim(ctx, p.session)
	if err != nil {
		return err
	}
	if row.ShimKey != p.key || state.Binding.Session != row.SessionID || state.Binding.Operation != row.ShimKey || state.Binding.Generation != row.RuntimeGeneration || state.Binding.Instance == "" {
		return ErrSessionShimConflict
	}
	if state.Binding.Journal != row.JournalID {
		// During post-placement binding the canonical receipt is already saved,
		// but only the pristine discriminator may still have an empty journal.
		if state.Binding.Journal != "" || state.Epoch != 0 || state.NextID != shimcodex.FirstID || len(state.Operations) != 0 || len(state.Inbox) != 0 || len(state.Partial) != 0 || state.InitializeID != 0 || state.Initialized || state.ThreadID != "" || state.ActiveTurn != "" || state.LastTerminal != "" || len(state.ServerRequests) != 0 || state.StreamOffset != 0 || state.PartialStart != 0 || state.Cursor != "" || state.ReplayHighWater != "" || state.ExitCursor != "" || state.Exit != nil {
			return ErrSessionShimConflict
		}
	}
	return nil
}

func (p *CodexProtocolStore) Load(ctx context.Context) (shimcodex.State, error) {
	var state shimcodex.State
	if err := p.store.currentCodexExecution(ctx, p.session); err != nil {
		return state, err
	}
	var protocol, revision, raw string
	// Bound the stored value before fetching the payload into the process.
	err := p.store.db.QueryRowContext(ctx, `SELECT protocol,revision,state_json FROM codex_shim_protocol
 WHERE session_id=? AND shim_key=? AND length(CAST(state_json AS BLOB))<=?`, p.session, p.key, p.maxBytes).Scan(&protocol, &revision, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		var found int
		e := p.store.db.QueryRowContext(ctx, `SELECT 1 FROM codex_shim_protocol WHERE session_id=?`, p.session).Scan(&found)
		if errors.Is(e, sql.ErrNoRows) {
			return state, shimcodex.ErrMissing
		}
		if e != nil {
			return state, e
		}
		return state, ErrSessionShimConflict
	}
	if err != nil {
		return state, err
	}
	if json.Unmarshal([]byte(raw), &state) != nil || protocol != shimcodex.Version || state.Version != protocol || strconv.FormatUint(state.Revision, 10) != revision || state.Binding.Session != p.session || state.Binding.Operation != p.key {
		return state, ErrSessionShimConflict
	}
	if err := p.validateBinding(ctx, state); err != nil {
		return state, err
	}
	return state, nil
}

func (p *CodexProtocolStore) Commit(ctx context.Context, previous uint64, state shimcodex.State) error {
	if previous == ^uint64(0) || state.Version != shimcodex.Version || state.Binding.Session != p.session || state.Binding.Operation != p.key || state.Revision != previous+1 {
		return ErrSessionShimConflict
	}
	if err := p.validateBinding(ctx, state); err != nil {
		return err
	}
	// Normal protocol transitions cannot mint or replace delivery proof.
	if previous == 0 {
		if state.Delivery != nil {
			return ErrSessionShimConflict
		}
	} else {
		current, err := p.Load(ctx)
		if err != nil || current.Revision != previous {
			return ErrSessionShimConflict
		}
		a, _ := json.Marshal(current.Delivery)
		b, _ := json.Marshal(state.Delivery)
		if !bytes.Equal(a, b) {
			return ErrSessionShimConflict
		}
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(raw) > p.maxBytes {
		return ErrSessionShimConflict
	}
	var result sql.Result
	if previous == 0 {
		result, err = p.store.db.ExecContext(ctx, `INSERT INTO codex_shim_protocol(session_id,shim_key,protocol,revision,state_json)
 SELECT ?,?,?,?,? WHERE EXISTS(SELECT 1 FROM session_shims WHERE session_id=? AND shim_key=? AND runtime_generation=?)
 AND NOT EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=?)
 ON CONFLICT(session_id) DO NOTHING`, p.session, p.key, state.Version, strconv.FormatUint(state.Revision, 10), string(raw), p.session, p.key, strconv.FormatUint(state.Binding.Generation, 10), p.session)
	} else {
		result, err = p.store.db.ExecContext(ctx, `UPDATE codex_shim_protocol SET revision=?,state_json=?
 WHERE session_id=? AND shim_key=? AND protocol=? AND revision=?
 AND EXISTS(SELECT 1 FROM session_shims WHERE session_id=? AND shim_key=? AND runtime_generation=?)
 AND NOT EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=?)`, strconv.FormatUint(state.Revision, 10), string(raw), p.session, p.key, state.Version, strconv.FormatUint(previous, 10), p.session, p.key, strconv.FormatUint(state.Binding.Generation, 10), p.session)
	}
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrSessionShimConflict
	}
	return nil
}

// Normal protocol handles cannot turn frozen old custody into a live runtime.
// The historical accounting issuer reads the raw ledger through its own
// bounded validator and does not use this operational loader.
func (s *Store) currentCodexExecution(ctx context.Context, session string) error {
	var replaced bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=?)`, session).Scan(&replaced); err != nil {
		return err
	}
	if replaced {
		return ErrSessionShimConflict
	}
	return nil
}

var _ shimcodex.Store = (*CodexProtocolStore)(nil)

// VerifiedCodexDelivery has no exported fields or public constructor. Only a
// separately assigned trusted store issuer may populate it after its atomic
// projection/outbox/receipt transaction and complete commit fence exist.
// The checked projection is committed with inbox removal by CommitDelivery.
type VerifiedCodexDelivery struct {
	issuer            *Store
	receiptID         string
	observationDigest [32]byte
	acceptedCursor    string
	replayHighWater   string
	deliveredCursor   string
	terminalIdentity  string
	terminal          shim.Exit
}

var ErrCodexDeliveryUnsupported = errors.New("trusted Codex delivery receipt unavailable")

func (r *VerifiedCodexDelivery) Validate(issuer *Store, state shimcodex.State, highwater string, terminal bool) error {
	if r == nil || issuer == nil || r.issuer != issuer || r.receiptID == "" || state.ReplayHighWater == "" || highwater != state.ReplayHighWater || r.replayHighWater != highwater || r.acceptedCursor != state.Cursor || r.deliveredCursor != state.Cursor || len(state.Inbox) != 0 || len(state.Partial) != 0 {
		return ErrCodexDeliveryUnsupported
	}
	prefix := state.Binding.Journal + ":"
	if state.Version != shimcodex.Version || state.Binding.Journal == "" || !strings.HasPrefix(state.Cursor, prefix) || !strings.HasPrefix(highwater, prefix) {
		return ErrCodexDeliveryUnsupported
	}
	accepted, err := strconv.ParseUint(strings.TrimPrefix(state.Cursor, prefix), 10, 64)
	if err != nil {
		return ErrCodexDeliveryUnsupported
	}
	high, err := strconv.ParseUint(strings.TrimPrefix(highwater, prefix), 10, 64)
	if err != nil || accepted < high {
		return ErrCodexDeliveryUnsupported
	}
	raw, err := json.Marshal(state)
	if err != nil || sha256.Sum256(raw) != r.observationDigest {
		return ErrCodexDeliveryUnsupported
	}
	for _, op := range state.Operations {
		if op.EffectUnknown || op.Phase == shimcodex.Intent || op.Phase == shimcodex.Attempted || op.Phase == shimcodex.Written {
			return ErrCodexDeliveryUnsupported
		}
	}
	for _, request := range state.ServerRequests {
		if !request.Written {
			return ErrCodexDeliveryUnsupported
		}
	}
	if terminal {
		if !strings.HasPrefix(state.ExitCursor, prefix) {
			return ErrCodexDeliveryUnsupported
		}
		exitCursor, err := strconv.ParseUint(strings.TrimPrefix(state.ExitCursor, prefix), 10, 64)
		if err != nil || exitCursor > accepted {
			return ErrCodexDeliveryUnsupported
		}
	}
	if terminal && (state.Exit == nil || state.ExitCursor == "" || r.terminalIdentity != state.Binding.Journal+":exit:"+state.ExitCursor || r.terminal != *state.Exit) {
		return ErrCodexDeliveryUnsupported
	}
	return nil
}
