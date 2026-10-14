package store

import (
	"database/sql"
	"errors"
	"time"
)

// SessionUsage is the per-session meter. Tokens and cost cover only the
// UsageTurns turns whose runtime reported usage; a session with Turns > 0 and
// UsageTurns == 0 is unmetered (for example a PTY CLI agent), not free.
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

// TurnUsage is one finished turn's contribution to the meter.
type TurnUsage struct {
	Failed       bool
	Reported     bool
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	Duration     time.Duration
}

// AddSessionTurnUsage folds one finished turn into the session's meter.
func (s *Store) AddSessionTurnUsage(sessionID string, u TurnUsage) error {
	var failed, reported int64
	if u.Failed {
		failed = 1
	}
	if u.Reported {
		reported = 1
	}
	_, err := s.db.Exec(`INSERT INTO session_usage(session_id, turns, failed_turns, usage_turns, input_tokens, output_tokens, cost_usd, turn_ms, updated_at)
		VALUES(?, 1, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET turns=turns+1, failed_turns=failed_turns+excluded.failed_turns,
		usage_turns=usage_turns+excluded.usage_turns, input_tokens=input_tokens+excluded.input_tokens,
		output_tokens=output_tokens+excluded.output_tokens, cost_usd=cost_usd+excluded.cost_usd,
		turn_ms=turn_ms+excluded.turn_ms, updated_at=excluded.updated_at`,
		sessionID, failed, reported, u.InputTokens, u.OutputTokens, u.CostUSD, u.Duration.Milliseconds(),
		time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// GetSessionUsage returns the session's meter, zero-valued when no turn has
// finished yet.
func (s *Store) GetSessionUsage(sessionID string) (SessionUsage, error) {
	u := SessionUsage{SessionID: sessionID}
	err := s.db.QueryRow(`SELECT turns, failed_turns, usage_turns, input_tokens, output_tokens, cost_usd, turn_ms, updated_at
		FROM session_usage WHERE session_id=?`, sessionID).
		Scan(&u.Turns, &u.FailedTurns, &u.UsageTurns, &u.InputTokens, &u.OutputTokens, &u.CostUSD, &u.TurnMS, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionUsage{SessionID: sessionID}, nil
	}
	return u, err
}
