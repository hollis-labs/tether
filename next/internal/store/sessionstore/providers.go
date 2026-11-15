package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/hollis-labs/tether/next/internal/store"
)

// ErrProviderMappingNotFound is returned for an unknown (session, owner, provider) mapping.
var ErrProviderMappingNotFound = errors.New("store: provider mapping not found")

// SessionProviderMapping maps a session to a provider's native session id ("" when none: the
// column is NULL).
type SessionProviderMapping struct {
	SessionID, Owner, Provider, NativeSessionID string
	CreatedAt, UpdatedAt                        string
}

const polMappingColumns = `session_id, owner, provider, native_session_id, created_at, updated_at`

func polScanMapping(scan func(...any) error) (SessionProviderMapping, error) {
	var m SessionProviderMapping
	var native sql.NullString
	if err := scan(&m.SessionID, &m.Owner, &m.Provider, &native, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return SessionProviderMapping{}, err
	}
	m.NativeSessionID = native.String
	return m, nil
}

// UpsertSessionProviderMapping stores the mapping ("" native id stores NULL); created_at is kept on
// update.
func (s *Store) UpsertSessionProviderMapping(ctx context.Context, sessionID, owner, provider, nativeSessionID string) error {
	if sessionID == "" || owner == "" || provider == "" {
		return fmt.Errorf("store: upsert session provider mapping: sessionID, owner and provider are required")
	}
	now := s.secondStamp()
	return s.exec(ctx, `INSERT INTO session_provider_mappings (`+polMappingColumns+`) VALUES (?, ?, ?, ?, ?, ?)
 ON CONFLICT (session_id, owner, provider) DO UPDATE SET native_session_id = excluded.native_session_id, updated_at = excluded.updated_at`,
		sessionID, owner, provider, nullText(nativeSessionID), now, now)
}

// GetSessionProviderMapping returns one mapping (ErrProviderMappingNotFound when absent).
func (s *Store) GetSessionProviderMapping(ctx context.Context, sessionID, owner, provider string) (SessionProviderMapping, error) {
	r := s.c.Reader()
	m, err := polScanMapping(r.QueryRowContext(ctx, rebind(r, `SELECT `+polMappingColumns+` FROM session_provider_mappings
 WHERE session_id = ? AND owner = ? AND provider = ?`), sessionID, owner, provider).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionProviderMapping{}, fmt.Errorf("%w: session=%s owner=%s provider=%s", ErrProviderMappingNotFound, sessionID, owner, provider)
	}
	return m, err
}

// ListSessionProviderMappings returns a session's mappings by provider, then owner (the columns are
// byte-ordered on both dialects).
func (s *Store) ListSessionProviderMappings(ctx context.Context, sessionID string) ([]SessionProviderMapping, error) {
	all, err := s.SessionProviderMappings(ctx, []string{sessionID})
	if err != nil {
		return nil, err
	}
	if all[sessionID] == nil {
		return []SessionProviderMapping{}, nil
	}
	return all[sessionID], nil
}

// SessionProviderMappings returns the mappings of many sessions, keyed by session.
func (s *Store) SessionProviderMappings(ctx context.Context, ids []string) (map[string][]SessionProviderMapping, error) {
	out := make(map[string][]SessionProviderMapping)
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, rebind(r, `SELECT `+polMappingColumns+` FROM session_provider_mappings
 WHERE session_id IN (`+placeholders(len(ids))+`) ORDER BY session_id, provider, owner`), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		m, err := polScanMapping(rows.Scan)
		if err != nil {
			return nil, err
		}
		out[m.SessionID] = append(out[m.SessionID], m)
	}
	return out, rows.Err()
}

// SessionUsage is a session's accumulated turn meter (class B, telemetry).
type SessionUsage struct {
	SessionID    string  `json:"session_id"`
	Turns        int64   `json:"turns"`
	FailedTurns  int64   `json:"failed_turns"`
	UsageTurns   int64   `json:"usage_turns"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	TurnMS       int64   `json:"turn_ms"`
	UpdatedAt    string  `json:"updated_at,omitempty"`
}

// TurnUsage is one turn's contribution to the meter.
type TurnUsage struct {
	Failed       bool
	Reported     bool
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	Duration     time.Duration
}

// AddSessionTurnUsage adds one turn to the session's meter (an atomic upsert, so concurrent turns
// all count), in a class B transaction.
func (s *Store) AddSessionTurnUsage(ctx context.Context, sessionID string, u TurnUsage) error {
	var failed, reported int64
	if u.Failed {
		failed = 1
	}
	if u.Reported {
		reported = 1
	}
	return s.all.B().WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO session_usage (session_id, turns, failed_turns, usage_turns, input_tokens, output_tokens, cost_usd, turn_ms, updated_at)
 VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT (session_id) DO UPDATE SET turns = session_usage.turns + 1, failed_turns = session_usage.failed_turns + excluded.failed_turns,
 usage_turns = session_usage.usage_turns + excluded.usage_turns, input_tokens = session_usage.input_tokens + excluded.input_tokens,
 output_tokens = session_usage.output_tokens + excluded.output_tokens, cost_usd = session_usage.cost_usd + excluded.cost_usd,
 turn_ms = session_usage.turn_ms + excluded.turn_ms, updated_at = excluded.updated_at`),
			sessionID, failed, reported, u.InputTokens, u.OutputTokens, u.CostUSD, u.Duration.Milliseconds(), s.now().Format(time.RFC3339Nano))
		return err
	})
}

// GetSessionUsage returns the session's meter (zero when none).
func (s *Store) GetSessionUsage(ctx context.Context, sessionID string) (SessionUsage, error) {
	u := SessionUsage{SessionID: sessionID}
	r := s.all.B().Reader()
	err := r.QueryRowContext(ctx, rebind(r, `SELECT turns, failed_turns, usage_turns, input_tokens, output_tokens, cost_usd, turn_ms, updated_at
 FROM session_usage WHERE session_id = ?`), sessionID).
		Scan(&u.Turns, &u.FailedTurns, &u.UsageTurns, &u.InputTokens, &u.OutputTokens, &u.CostUSD, &u.TurnMS, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionUsage{SessionID: sessionID}, nil
	}
	return u, err
}
