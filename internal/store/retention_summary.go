package store

import (
	"context"
	"fmt"
	"time"
)

// RetentionSummary is the report-only view of delivery backlog and age that a
// retention policy would act on (CW-20261030-0177). It reads messaging_deliveries
// and changes nothing: no state moves to expired and nothing is purged. The
// thresholds are the proposed policy, not an enforced one.
type RetentionSummary struct {
	GeneratedAt string           `json:"generated_at"`
	ByStatus    map[string]int   `json:"by_status"`
	Groups      []RetentionGroup `json:"groups"`
}

// RetentionGroup is one cut of the backlog: how many deliveries, how old, and
// which recipients hold most of them.
type RetentionGroup struct {
	Name          string               `json:"name"`
	Description   string               `json:"description"`
	Count         int                  `json:"count"`
	Oldest        string               `json:"oldest,omitempty"`
	TopRecipients []RetentionRecipient `json:"top_recipients,omitempty"`
}

// RetentionRecipient is a per-recipient count inside one group.
type RetentionRecipient struct {
	Recipient string `json:"recipient"`
	Count     int    `json:"count"`
}

// Proposed retention thresholds (report-only until a policy is approved).
const (
	retentionStalePending  = 14 * 24 * time.Hour
	retentionDeadLettered  = 30 * 24 * time.Hour
	retentionSettledPurge  = 90 * 24 * time.Hour
	retentionTopRecipients = 10
)

type retentionGroupSpec struct {
	name, description, where string
	args                     []any
}

// RetentionSummary builds the report as of now.
func (s *Store) RetentionSummary(ctx context.Context, now time.Time) (RetentionSummary, error) {
	ts := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339Nano) }
	out := RetentionSummary{GeneratedAt: now.UTC().Format(time.RFC3339Nano), ByStatus: map[string]int{}}

	// s.db has MaxOpenConns=1: every Rows is drained and closed before the next query.
	rows, err := s.db.QueryContext(ctx, `SELECT status, count(*) FROM messaging_deliveries GROUP BY status`)
	if err != nil {
		return out, fmt.Errorf("store: retention summary: status counts: %w", err)
	}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close() //nolint:errcheck
			return out, fmt.Errorf("store: retention summary: scan status: %w", err)
		}
		out.ByStatus[status] = n
	}
	err = rows.Err()
	rows.Close() //nolint:errcheck
	if err != nil {
		return out, fmt.Errorf("store: retention summary: status counts: %w", err)
	}

	specs := []retentionGroupSpec{
		{"stale_pending", "pending or retry_scheduled for more than 14 days: candidates for an expired state under the proposed policy (nothing is changed)",
			`status IN ('pending','retry_scheduled') AND created_at < ?`, []any{ts(retentionStalePending)}},
		{"retry_scheduled", "deliveries currently waiting on a wake retry, any age",
			`status = 'retry_scheduled'`, nil},
		{"dead_lettered_over_30d", "dead-lettered more than 30 days ago (redrivable until purged; never purge-eligible today)",
			`status = 'dead_lettered' AND updated_at < ?`, []any{ts(retentionDeadLettered)}},
		{"settled_over_90d", "delivered or canceled more than 90 days ago: purge-eligible under the proposed policy",
			`status IN ('delivered','canceled') AND updated_at < ?`, []any{ts(retentionSettledPurge)}},
	}
	for _, sp := range specs {
		g := RetentionGroup{Name: sp.name, Description: sp.description}
		var oldest *string
		if err := s.db.QueryRowContext(ctx, `SELECT count(*), min(created_at) FROM messaging_deliveries WHERE `+sp.where, sp.args...).Scan(&g.Count, &oldest); err != nil {
			return out, fmt.Errorf("store: retention summary: %s: %w", sp.name, err)
		}
		if oldest != nil {
			g.Oldest = *oldest
		}
		if g.Count > 0 {
			rs, err := s.db.QueryContext(ctx, `SELECT recipient_urn, count(*) c FROM messaging_deliveries WHERE `+sp.where+` GROUP BY recipient_urn ORDER BY c DESC, recipient_urn LIMIT ?`, //nolint:gosec // G202: sp.where is one of the constant clauses above, never caller input
				append(append([]any{}, sp.args...), retentionTopRecipients)...)
			if err != nil {
				return out, fmt.Errorf("store: retention summary: %s recipients: %w", sp.name, err)
			}
			for rs.Next() {
				var r RetentionRecipient
				if err := rs.Scan(&r.Recipient, &r.Count); err != nil {
					rs.Close() //nolint:errcheck
					return out, fmt.Errorf("store: retention summary: scan recipient: %w", err)
				}
				g.TopRecipients = append(g.TopRecipients, r)
			}
			err = rs.Err()
			rs.Close() //nolint:errcheck
			if err != nil {
				return out, fmt.Errorf("store: retention summary: %s recipients: %w", sp.name, err)
			}
		}
		out.Groups = append(out.Groups, g)
	}
	return out, nil
}
