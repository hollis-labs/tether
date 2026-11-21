package store

import (
	"context"
	"time"
)

// KitSeatRetention is how long a kit-seat registration is kept after it was
// last re-registered. Stale rows stay listable (include_stale) until then.
const KitSeatRetention = 7 * 24 * time.Hour

// KitSeat is one external kit-booted seat registration (CW-20261121-0114).
type KitSeat struct {
	AgentURN          string
	Session           string
	RunID             string
	TaskID            string
	WorkstreamID      string
	Host              string
	DisplayName       string
	Summary           string
	ProviderSessionID string
	FirstSeenAt       time.Time
	LastSeenAt        time.Time
}

// UpsertKitSeat records a registration at now. Re-registering refreshes the
// descriptive facts and last_seen_at; empty facts never erase known ones.
// Registrations older than KitSeatRetention are pruned in the same call.
func (s *Store) UpsertKitSeat(ctx context.Context, k KitSeat, now time.Time) error {
	at := now.UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO kit_seats(agent_urn,session,run_id,task_id,workstream_id,host,display_name,summary,provider_session_id,first_seen_at,last_seen_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(agent_urn,session) DO UPDATE SET
		 run_id=COALESCE(NULLIF(excluded.run_id,''),run_id),
		 task_id=COALESCE(NULLIF(excluded.task_id,''),task_id),
		 workstream_id=COALESCE(NULLIF(excluded.workstream_id,''),workstream_id),
		 host=COALESCE(NULLIF(excluded.host,''),host),
		 display_name=COALESCE(NULLIF(excluded.display_name,''),display_name),
		 summary=COALESCE(NULLIF(excluded.summary,''),summary),
		 provider_session_id=COALESCE(NULLIF(excluded.provider_session_id,''),provider_session_id),
		 last_seen_at=excluded.last_seen_at`,
		k.AgentURN, k.Session, k.RunID, k.TaskID, k.WorkstreamID, k.Host, k.DisplayName, k.Summary, k.ProviderSessionID, at, at); err != nil {
		return err
	}
	cutoff := now.Add(-KitSeatRetention).UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `DELETE FROM kit_seats WHERE last_seen_at < ?`, cutoff); err != nil {
		return err
	}
	return tx.Commit()
}

// ListKitSeats returns registrations last seen at or after since, newest first.
func (s *Store) ListKitSeats(ctx context.Context, since time.Time, limit int) ([]KitSeat, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT agent_urn,session,run_id,task_id,workstream_id,host,display_name,summary,provider_session_id,first_seen_at,last_seen_at
		FROM kit_seats WHERE last_seen_at >= ? ORDER BY last_seen_at DESC, agent_urn, session LIMIT ?`, since.UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KitSeat
	for rows.Next() {
		var k KitSeat
		var first, last string
		if err := rows.Scan(&k.AgentURN, &k.Session, &k.RunID, &k.TaskID, &k.WorkstreamID, &k.Host, &k.DisplayName, &k.Summary, &k.ProviderSessionID, &first, &last); err != nil {
			return nil, err
		}
		k.FirstSeenAt, _ = time.Parse(time.RFC3339Nano, first)
		k.LastSeenAt, _ = time.Parse(time.RFC3339Nano, last)
		out = append(out, k)
	}
	return out, rows.Err()
}
