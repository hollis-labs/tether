package eventstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"
)

// MetricsQuery selects tool_call_end events for aggregation. BoundsMs are the histogram bucket
// upper bounds in ms (telemetry.HistogramBoundsMs in the old tree), ascending.
type MetricsQuery struct {
	Tool, Upstream, Mode string
	ByMode               bool
	Since, Until         time.Time
	Limit                int
	BoundsMs             []int
}

// MetricGroup is one aggregated row. Each histogram has one count per bound (calls at or under it)
// and a sum; the event slice turns them into telemetry.Histogram.
type MetricGroup struct {
	Tool, Upstream, Outcome, Mode string
	Calls, ArgsBytes, ResultBytes int64
	MetadataSamples               int64 // calls whose payload carries args_bytes (newer producers)
	Duration, Gateway, Forward    MetricHistogram
}

// MetricHistogram is one latency field's sum and cumulative bucket counts.
type MetricHistogram struct {
	SumMs  int64
	Counts []int64 // len(BoundsMs)
}

// metricsSQL builds the per-dialect aggregation. On SQLite it is the old query (json_extract and
// json_type, guarded by json_valid). On Postgres the payload is cast once to jsonb in a subquery
// guarded by IS JSON (Postgres 16+, so an invalid payload is skipped, not an error); "key present" is
// jsonb_exists (the ? operator's function form: Rebind would read a bare ? as a placeholder), which,
// like json_type, is true for a JSON null; and ok counts when it is true or 1, matching SQLite's
// json_extract(...)=1. A non-numeric value in a numeric field fails the cast on Postgres, where SQLite
// summed it as 0; producers write numbers.
func metricsSQL(d gostore.Driver, q MetricsQuery) (string, []any) {
	pg := d == gostore.DriverPostgres
	text := func(f string) string {
		if pg {
			return "(p->>'" + f + "')"
		}
		return "json_extract(payload_json,'$." + f + "')"
	}
	num := func(f string) string {
		if pg {
			return "(p->>'" + f + "')::numeric"
		}
		return "json_extract(payload_json,'$." + f + "')"
	}
	present := func(f string) string {
		if pg {
			return "jsonb_exists(p, '" + f + "')"
		}
		return "json_type(payload_json,'$." + f + "') IS NOT NULL"
	}
	okExpr := "json_extract(payload_json,'$.ok')=1"
	if pg {
		okExpr = "(p->'ok' = 'true'::jsonb OR p->'ok' = '1'::jsonb)"
	}
	sum := func(expr string) string {
		if pg {
			return "COALESCE(SUM(" + expr + "),0)::bigint"
		}
		return "COALESCE(SUM(" + expr + "),0)"
	}
	modeExpr, groupBy := "''", "1,2,3"
	if q.ByMode {
		modeExpr, groupBy = "COALESCE("+text("discovery_mode")+",'')", "1,2,3,4"
	}
	var b strings.Builder
	b.WriteString(`SELECT COALESCE(` + text("tool_name") + `,''), COALESCE(` + text("server") + `,''),
 CASE WHEN ` + okExpr + ` THEN 'ok' ELSE COALESCE(NULLIF(` + text("error_class") + `,''),'upstream_error') END,
 ` + modeExpr + `, COUNT(*), ` + sum(num("args_bytes")) + `, ` + sum(num("result_bytes")) + `,
 ` + sum("CASE WHEN "+present("args_bytes")+" THEN 1 ELSE 0 END"))
	for index, field := range []string{"duration_ms", "gateway_ms", "forward_ms"} {
		expr := "COALESCE(" + num(field) + ",0)"
		b.WriteString(", " + sum(expr))
		for _, bound := range q.BoundsMs {
			cond := fmt.Sprintf("%s <= %d", expr, bound)
			if index > 0 {
				cond = present(field) + " AND " + cond
			}
			b.WriteString(", " + sum("CASE WHEN "+cond+" THEN 1 ELSE 0 END"))
		}
	}
	if pg {
		b.WriteString(` FROM (SELECT at, payload_json::jsonb AS p FROM events WHERE kind = 'tool_call_end' AND payload_json IS JSON) e WHERE 1=1`)
	} else {
		b.WriteString(` FROM events WHERE kind = 'tool_call_end' AND json_valid(payload_json)`)
	}
	var args []any
	for _, f := range []struct{ field, value string }{{"tool_name", q.Tool}, {"server", q.Upstream}, {"discovery_mode", q.Mode}} {
		if f.value != "" {
			b.WriteString(" AND " + text(f.field) + " = ?")
			args = append(args, f.value)
		}
	}
	// at is RFC 3339 text with a variable number of fraction digits; normalize it to nine digits so
	// the range compares as text in time order (the old store's expression; substr, replace and || are
	// the same on both dialects).
	normalizedAt := `(substr(at,1,19) || '.' || substr(replace(substr(at,21),'Z','') || '000000000',1,9) || 'Z')`
	const fixedUTC = "2006-01-02T15:04:05.000000000Z"
	if !q.Since.IsZero() {
		b.WriteString(" AND " + normalizedAt + " >= ?")
		args = append(args, q.Since.UTC().Format(fixedUTC))
	}
	if !q.Until.IsZero() {
		b.WriteString(" AND " + normalizedAt + " < ?")
		args = append(args, q.Until.UTC().Format(fixedUTC))
	}
	b.WriteString(" GROUP BY " + groupBy + " ORDER BY COUNT(*) DESC,1,2,3,4 LIMIT ?")
	args = append(args, q.Limit)
	return b.String(), args
}

// MaxMetricGroups bounds QueryToolCallMetrics when q.Limit is out of range (the caller asks for
// one more than it shows, to detect truncation).
const MaxMetricGroups = 1000

// QueryToolCallMetrics aggregates tool_call_end events by tool, upstream, outcome (and discovery
// mode when ByMode).
func (s *Store) QueryToolCallMetrics(ctx context.Context, q MetricsQuery) ([]MetricGroup, error) {
	if q.Limit < 1 || q.Limit > MaxMetricGroups+1 {
		q.Limit = MaxMetricGroups + 1
	}
	r := s.b.Reader()
	sqlText, args := metricsSQL(r.Dialect().Name(), q)
	rows, err := r.QueryContext(ctx, rebind(r, sqlText), args...)
	if err != nil {
		return nil, fmt.Errorf("query tool metrics: %w", err)
	}
	defer func() { _ = rows.Close() }()
	groups := []MetricGroup{}
	for rows.Next() {
		var g MetricGroup
		targets := []any{&g.Tool, &g.Upstream, &g.Outcome, &g.Mode, &g.Calls, &g.ArgsBytes, &g.ResultBytes, &g.MetadataSamples}
		for _, h := range []*MetricHistogram{&g.Duration, &g.Gateway, &g.Forward} {
			h.Counts = make([]int64, len(q.BoundsMs))
			targets = append(targets, &h.SumMs)
			for i := range h.Counts {
				targets = append(targets, &h.Counts[i])
			}
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("scan tool metrics: %w", err)
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}
