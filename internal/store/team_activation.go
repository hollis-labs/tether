package store

import (
	"context"
	"encoding/json"
	"github.com/hollis-labs/tether/internal/launchprofile"
)

// RecordTeamEvidence keeps the first observation at a lifecycle boundary. Retries
// and daemon restarts cannot silently replace the attested baseline.
func (s *Store) RecordTeamEvidence(ctx context.Context, kind, key string, e launchprofile.LaunchEvidence) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = s.DB().ExecContext(ctx, `INSERT OR IGNORE INTO team_activation_evidence(kind,evidence_key,payload,recorded_at) VALUES(?,?,?,strftime('%Y-%m-%dT%H:%M:%fZ','now'))`, kind, key, string(raw))
	return err
}
func (s *Store) TeamEvidence(ctx context.Context, kind, key string) (*launchprofile.LaunchEvidence, error) {
	var raw string
	if err := s.DB().QueryRowContext(ctx, `SELECT payload FROM team_activation_evidence WHERE kind=? AND evidence_key=?`, kind, key).Scan(&raw); err != nil {
		return nil, err
	}
	var e launchprofile.LaunchEvidence
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		return nil, err
	}
	return &e, nil
}
