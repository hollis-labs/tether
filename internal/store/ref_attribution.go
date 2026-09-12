package store

// ref_attribution.go — whether a session's proxy could ever produce a
// source='proxy' ref.
//
// S5 of SP-20260912-0001 (CW-20260912-0063). Migration 0025 states why this is
// a recorded fact rather than something inferred from launch_id at read time;
// the short version is that the inference is false for every session that
// exists today.

import (
	"fmt"
	"time"
)

// Ref attribution values. See migration 0025 for what each one asserts.
const (
	// RefAttributionProxy — planted with --session AND --extract-refs. This
	// session's proxy can produce refs.
	RefAttributionProxy = "proxy"
	// RefAttributionNone — planted with --session but not --extract-refs.
	// The proxy knows which session it serves and was not asked to record.
	RefAttributionNone = "none"
	// RefAttributionUnlaunched — nothing was planted for this session.
	// POST /sessions/bootstrap: an external caller owns the process, so no
	// proxy carries this session's id.
	RefAttributionUnlaunched = "unlaunched"
	// RefAttributionUnknown is what a NULL column reads as. It is NOT a
	// synonym for "none": "none" says we know extraction was off, "unknown"
	// says nothing recorded what was planted. Rows predating migration 0025
	// are unknown and are deliberately not backfilled.
	RefAttributionUnknown = "unknown"
)

// CanProduceProxyRefs reports whether a session with this attribution could
// ever yield a source='proxy' ref.
//
// Only RefAttributionProxy can. Note what this means for a digest: for
// everything else, an empty proxy column is a fact about configuration and
// never a fact about the session's activity. Unknown answers false because an
// unproven capability is not a capability -- but a consumer must not turn that
// false into "this session produced nothing", which is the distinction the
// whole column exists to carry.
func CanProduceProxyRefs(attribution string) bool {
	return attribution == RefAttributionProxy
}

// NormalizeRefAttribution maps the stored value (including the empty string a
// NULL column scans as) onto the rendering vocabulary.
func NormalizeRefAttribution(v string) string {
	if v == "" {
		return RefAttributionUnknown
	}
	return v
}

// SetSessionRefAttribution records what was actually planted for a session.
//
// Called from the planting site with the value the planting itself produced,
// never computed from the session row -- see app.MuxMCPPlant, which returns
// the argv and the attribution together precisely so these two cannot be
// derived independently and drift apart.
func (s *Store) SetSessionRefAttribution(sessionID, attribution string) error {
	if sessionID == "" {
		return fmt.Errorf("set ref attribution: session id required")
	}
	if attribution == "" {
		return fmt.Errorf("set ref attribution for %q: attribution required (pass %q explicitly rather than clearing the column, which means 'unknown')", sessionID, RefAttributionNone)
	}
	res, err := s.db.Exec(
		`UPDATE sessions SET ref_attribution=?, updated_at=? WHERE id=?`,
		attribution, time.Now().UTC().Format(time.RFC3339), sessionID,
	)
	if err != nil {
		return fmt.Errorf("set ref attribution for %q: %w", sessionID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set ref attribution for %q: %w", sessionID, err)
	}
	if n == 0 {
		return ErrSessionNotFound
	}
	return nil
}
