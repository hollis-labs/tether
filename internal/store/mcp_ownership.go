package store

import "context"

// MCPUpstreamSessionCounts reports captured ownership for active sessions.
// Old or unplanted sessions remain unknown rather than claiming serve-once.
func (s *Store) MCPUpstreamSessionCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(NULLIF(json_extract(p.policy_json,'$.upstream_ownership'),''),'unknown'),COUNT(*)
 FROM sessions s LEFT JOIN session_mcp_policy p ON p.session_id=s.id
 WHERE s.state IN ('launching','running') AND s.provider_kind!='api'
 GROUP BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{"legacy_proxy": 0, "daemon": 0, "unknown": 0}
	for rows.Next() {
		var key string
		var count int
		if err := rows.Scan(&key, &count); err != nil {
			return nil, err
		}
		if key != "legacy_proxy" && key != "daemon" {
			key = "unknown"
		}
		counts[key] += count
	}
	return counts, rows.Err()
}
