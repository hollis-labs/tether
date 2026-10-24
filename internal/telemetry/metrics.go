package telemetry

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// HistogramBoundsMs defines cumulative latency buckets shared by introspection
// and OTel. The final JSON bucket has a null upper bound, meaning +infinity.
var HistogramBoundsMs = [...]int64{5, 25, 100, 500, 1000, 5000}

const MaxMetricGroups = 1000

// MaxOTelCallSeries bounds process memory even when callers name unknown tools.
// Later combinations share an overflow series; durable queries remain exact.
const MaxOTelCallSeries = 1000

// MaxOTelDeniedSeries reserves a separate budget so unknown/excluded names
// cannot crowd out dispatched tools. The remaining slots are non-denied series.
const MaxOTelDeniedSeries = 100

type Bucket struct {
	UpperMs *int64 `json:"upper_ms"`
	Count   int64  `json:"count"`
}
type Histogram struct {
	Count   int64    `json:"count"`
	SumMs   int64    `json:"sum_ms"`
	Buckets []Bucket `json:"buckets"`
}
type MetricGroup struct {
	Tool     string `json:"tool"`
	Upstream string `json:"upstream"`
	Outcome  string `json:"outcome"`
	// Mode is the discovery mode (flat|search); set only when grouping by mode.
	// An empty value under group_by=mode means the event predates the field.
	Mode            string    `json:"mode,omitempty"`
	Calls           int64     `json:"calls"`
	ArgsBytes       int64     `json:"args_bytes"`
	ResultBytes     int64     `json:"result_bytes"`
	MetadataSamples int64     `json:"metadata_samples"`
	Duration        Histogram `json:"duration"`
	Gateway         Histogram `json:"gateway"`
	Forward         Histogram `json:"forward"`
}
type MetricsRequest struct {
	Tool     string `json:"tool,omitempty"`
	Upstream string `json:"upstream,omitempty"`
	Since    string `json:"since,omitempty"`
	Until    string `json:"until,omitempty"`
	// Mode filters to one discovery mode (flat|search). GroupBy "mode" adds the
	// mode as a grouping dimension. Compact returns CompactGroup rows (averages
	// and p50/p95 bounds, no bucket arrays) and defaults Limit to 25. Limit caps
	// the groups returned, highest call count first (CW-20261024-0048).
	Mode    string `json:"mode,omitempty"`
	GroupBy string `json:"group_by,omitempty"`
	Compact bool   `json:"compact,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}
type MetricsQuery struct {
	Tool, Upstream, Mode string
	ByMode               bool
	Since, Until         time.Time
	Limit                int
}

// CompactGroup is the token-cheap form of MetricGroup. P50LeMs/P95LeMs are the
// upper bound in ms of the histogram bucket holding that percentile (nil means
// above the last bound, 5000ms); they are bucket bounds, not exact quantiles.
type CompactGroup struct {
	Tool           string `json:"tool"`
	Upstream       string `json:"upstream"`
	Outcome        string `json:"outcome"`
	Mode           string `json:"mode,omitempty"`
	Calls          int64  `json:"calls"`
	AvgArgsBytes   int64  `json:"avg_args_bytes"`
	AvgResultBytes int64  `json:"avg_result_bytes"`
	AvgMs          int64  `json:"avg_ms"`
	P50LeMs        *int64 `json:"p50_le_ms"`
	P95LeMs        *int64 `json:"p95_le_ms"`
}
type MetricsResponse struct {
	Groups    []MetricGroup  `json:"groups"`
	Compact   []CompactGroup `json:"compact,omitempty"`
	Truncated bool           `json:"truncated"`
	Window    string         `json:"window"`
}

// Compact reduces g to its token-cheap summary.
func (g MetricGroup) Compact() CompactGroup {
	c := CompactGroup{Tool: g.Tool, Upstream: g.Upstream, Outcome: g.Outcome, Mode: g.Mode, Calls: g.Calls}
	if g.Calls > 0 {
		c.AvgArgsBytes = g.ArgsBytes / g.Calls
		c.AvgResultBytes = g.ResultBytes / g.Calls
		c.AvgMs = g.Duration.SumMs / g.Calls
	}
	c.P50LeMs = g.Duration.percentileBound(0.50)
	c.P95LeMs = g.Duration.percentileBound(0.95)
	return c
}

// percentileBound returns the upper bound of the first cumulative bucket that
// holds at least fraction p of the calls, or nil when only the +Inf bucket does.
func (h Histogram) percentileBound(p float64) *int64 {
	if h.Count == 0 {
		return nil
	}
	target := int64(float64(h.Count)*p + 0.999999)
	if target < 1 {
		target = 1
	}
	for _, b := range h.Buckets {
		if b.Count >= target {
			return b.UpperMs
		}
	}
	return nil
}

// DefaultCompactGroups is the group cap for compact responses without a limit.
const DefaultCompactGroups = 25

type MetricsSource interface {
	QueryToolCallMetrics(context.Context, MetricsQuery) ([]MetricGroup, error)
}
type QueryError struct{ Message string }

func (e *QueryError) Error() string { return e.Message }

// QueryService queries only durable completed calls; starts never inflate counts.
// The result survives daemon restarts and is bounded by event retention.
type QueryService struct{ Source MetricsSource }

func (s QueryService) ReadMetrics(ctx context.Context, req MetricsRequest) (MetricsResponse, error) {
	q := MetricsQuery{Tool: req.Tool, Upstream: req.Upstream, Mode: req.Mode, Limit: MaxMetricGroups + 1}
	if len(req.Tool) > 256 || len(req.Upstream) > 256 {
		return MetricsResponse{}, &QueryError{"tool/upstream identifier too long"}
	}
	switch req.Mode {
	case "", "flat", "search":
	default:
		return MetricsResponse{}, &QueryError{"mode must be flat or search"}
	}
	switch req.GroupBy {
	case "":
	case "mode":
		q.ByMode = true
	default:
		return MetricsResponse{}, &QueryError{"group_by must be mode"}
	}
	if req.Limit < 0 || req.Limit > MaxMetricGroups {
		return MetricsResponse{}, &QueryError{fmt.Sprintf("limit must be between 1 and %d", MaxMetricGroups)}
	}
	limitCap := MaxMetricGroups
	if req.Compact {
		limitCap = DefaultCompactGroups
	}
	if req.Limit > 0 {
		limitCap = req.Limit
	}
	for _, field := range []struct {
		name, value string
		dst         *time.Time
	}{{"since", req.Since, &q.Since}, {"until", req.Until, &q.Until}} {
		if field.value != "" {
			stamp, err := time.Parse(time.RFC3339Nano, field.value)
			if err != nil {
				return MetricsResponse{}, &QueryError{fmt.Sprintf("%s must be an RFC3339 timestamp", field.name)}
			}
			*field.dst = stamp
		}
	}
	if !q.Since.IsZero() && !q.Until.IsZero() && !q.Since.Before(q.Until) {
		return MetricsResponse{}, &QueryError{"since must precede until"}
	}
	groups, err := s.Source.QueryToolCallMetrics(ctx, q)
	if err != nil {
		return MetricsResponse{}, err
	}
	out := MetricsResponse{Groups: groups, Window: "retained_events"}
	if len(out.Groups) > limitCap {
		out.Groups = out.Groups[:limitCap]
		out.Truncated = true
	}
	if out.Groups == nil {
		out.Groups = []MetricGroup{}
	}
	if req.Compact {
		out.Compact = make([]CompactGroup, 0, len(out.Groups))
		for _, g := range out.Groups {
			out.Compact = append(out.Compact, g.Compact())
		}
		out.Groups = []MetricGroup{}
	}
	return out, nil
}

// Metrics exports process-lifetime completed-call observations. Identity and
// argument data are never labels. Durable introspection is a separate view.
type Metrics struct {
	mu                         sync.Mutex
	series                     map[metricLabels]struct{}
	deniedSeries               int
	calls                      metric.Int64Counter
	argsBytes, resultBytes     metric.Int64Counter
	duration, gateway, forward metric.Float64Histogram
}

type metricLabels struct{ tool, upstream, outcome string }

func NewMetrics(meter metric.Meter) (*Metrics, error) {
	m := &Metrics{series: make(map[metricLabels]struct{})}
	var err error
	if m.calls, err = meter.Int64Counter("tether.tool.calls", metric.WithDescription("Completed tool calls, including rejected calls")); err != nil {
		return nil, err
	}
	if m.argsBytes, err = meter.Int64Counter("tether.tool.args.bytes", metric.WithUnit("By")); err != nil {
		return nil, err
	}
	if m.resultBytes, err = meter.Int64Counter("tether.tool.result.bytes", metric.WithUnit("By")); err != nil {
		return nil, err
	}
	bounds := make([]float64, len(HistogramBoundsMs))
	for i, b := range HistogramBoundsMs {
		bounds[i] = float64(b)
	}
	for _, h := range []struct {
		name string
		dst  *metric.Float64Histogram
	}{{"tether.tool.duration", &m.duration}, {"tether.tool.gateway", &m.gateway}, {"tether.tool.forward", &m.forward}} {
		if *h.dst, err = meter.Float64Histogram(h.name, metric.WithUnit("ms"), metric.WithExplicitBucketBoundaries(bounds...)); err != nil {
			return nil, err
		}
	}
	return m, nil
}
func (m *Metrics) Observe(ctx context.Context, call events.ToolCallEvent) {
	if m == nil {
		return
	}
	outcome := string(call.ErrorClass)
	if call.OK {
		outcome = "ok"
	} else if outcome == "" {
		outcome = string(events.ToolErrorUpstream)
	}
	key := metricLabels{boundedMetricLabel(call.ToolName), boundedMetricLabel(call.Server), boundedMetricLabel(outcome)}
	m.mu.Lock()
	if _, known := m.series[key]; !known {
		denied := outcome == string(events.ToolErrorDenied)
		available := m.deniedSeries < MaxOTelDeniedSeries
		if !denied {
			available = len(m.series)-m.deniedSeries < MaxOTelCallSeries-MaxOTelDeniedSeries
		}
		if available {
			m.series[key] = struct{}{}
			if denied {
				m.deniedSeries++
			}
		} else {
			key = metricLabels{"_other", "_other", "_other"}
		}
	}
	m.mu.Unlock()
	labels := []attribute.KeyValue{attribute.String("tool", key.tool), attribute.String("upstream", key.upstream), attribute.String("outcome", key.outcome)}
	opts := metric.WithAttributes(labels...)
	m.calls.Add(ctx, 1, opts)
	m.duration.Record(ctx, float64(call.DurationMs), opts)
	m.gateway.Record(ctx, float64(call.GatewayMs), opts)
	m.forward.Record(ctx, float64(call.ForwardMs), opts)
	m.argsBytes.Add(ctx, call.ArgsBytes, opts)
	m.resultBytes.Add(ctx, call.ResultBytes, opts)
}

func boundedMetricLabel(value string) string {
	if len(value) > 256 {
		return "_other"
	}
	return value
}

var processMetrics, processMetricsError = NewMetrics(otel.Meter("tether/telemetry"))
