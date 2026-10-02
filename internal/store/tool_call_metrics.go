package store

import (
	"context"
	"fmt"
	"github.com/hollis-labs/tether/internal/telemetry"
)

// QueryToolCallMetrics aggregates durable completed calls before applying the
// group limit. It never scans argument/result values or the proxy projection.
func (s *Store) QueryToolCallMetrics(ctx context.Context, q telemetry.MetricsQuery) ([]telemetry.MetricGroup, error) {
	sqlText := `SELECT COALESCE(json_extract(payload_json,'$.tool_name'),''),
 COALESCE(json_extract(payload_json,'$.server'),''),
 CASE WHEN json_extract(payload_json,'$.ok')=1 THEN 'ok' ELSE COALESCE(NULLIF(json_extract(payload_json,'$.error_class'),''),'upstream_error') END,
 COUNT(*),COALESCE(SUM(json_extract(payload_json,'$.args_bytes')),0),COALESCE(SUM(json_extract(payload_json,'$.result_bytes')),0),
 SUM(CASE WHEN json_type(payload_json,'$.args_bytes') IS NOT NULL THEN 1 ELSE 0 END)`
	fields := []string{"duration_ms", "gateway_ms", "forward_ms"}
	for index, field := range fields {
		expr := "COALESCE(json_extract(payload_json,'$." + field + "'),0)"
		sqlText += ", SUM(" + expr + ")"
		for _, bound := range telemetry.HistogramBoundsMs {
			condition := fmt.Sprintf("%s <= %d", expr, bound)
			if index > 0 {
				condition = "json_type(payload_json,'$." + field + "') IS NOT NULL AND " + condition
			}
			sqlText += ", SUM(CASE WHEN " + condition + " THEN 1 ELSE 0 END)"
		}
	}
	sqlText += ` FROM events WHERE kind='tool_call_end' AND json_valid(payload_json)`
	var args []any
	for _, filter := range []struct{ path, value string }{{"tool_name", q.Tool}, {"server", q.Upstream}} {
		if filter.value != "" {
			sqlText += " AND json_extract(payload_json,'$." + filter.path + "')=?"
			args = append(args, filter.value)
		}
	}
	// Stored timestamps use RFC3339Nano, whose variable fraction width is not
	// lexically ordered at fractional boundaries. Pad to nine digits in UTC.
	normalizedAt := `(substr(at,1,19) || '.' || substr(replace(substr(at,21),'Z','') || '000000000',1,9) || 'Z')`
	const fixedUTC = "2006-01-02T15:04:05.000000000Z"
	if !q.Since.IsZero() {
		sqlText += " AND " + normalizedAt + ">=?"
		args = append(args, q.Since.UTC().Format(fixedUTC))
	}
	if !q.Until.IsZero() {
		sqlText += " AND " + normalizedAt + "<?"
		args = append(args, q.Until.UTC().Format(fixedUTC))
	}
	sqlText += " GROUP BY 1,2,3 ORDER BY 1,2,3 LIMIT ?"
	if q.Limit < 1 || q.Limit > telemetry.MaxMetricGroups+1 {
		q.Limit = telemetry.MaxMetricGroups + 1
	}
	args = append(args, q.Limit)
	// All SQL fragments and JSON paths are fixed; selectors are parameterized.
	//nolint:gosec // controlled assembly, no authored SQL fragments
	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("query tool metrics: %w", err)
	}
	defer rows.Close()
	groups := []telemetry.MetricGroup{}
	for rows.Next() {
		var group telemetry.MetricGroup
		targets := []any{&group.Tool, &group.Upstream, &group.Outcome, &group.Calls, &group.ArgsBytes, &group.ResultBytes, &group.MetadataSamples}
		histograms := []*telemetry.Histogram{&group.Duration, &group.Gateway, &group.Forward}
		for _, hist := range histograms {
			hist.Buckets = make([]telemetry.Bucket, len(telemetry.HistogramBoundsMs)+1)
			targets = append(targets, &hist.SumMs)
			for i, bound := range telemetry.HistogramBoundsMs {
				b := bound
				hist.Buckets[i].UpperMs = &b
				targets = append(targets, &hist.Buckets[i].Count)
			}
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("scan tool metrics: %w", err)
		}
		for i, hist := range histograms {
			hist.Count = group.Calls
			if i > 0 {
				hist.Count = group.MetadataSamples
			}
			hist.Buckets[len(hist.Buckets)-1].Count = hist.Count
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return groups, nil
}
