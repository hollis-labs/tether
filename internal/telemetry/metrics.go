package telemetry

import (
	"context"
	"fmt"
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
	Tool            string    `json:"tool"`
	Upstream        string    `json:"upstream"`
	Outcome         string    `json:"outcome"`
	Calls           int64     `json:"calls"`
	ArgsBytes       int64     `json:"args_bytes"`
	ResultBytes     int64     `json:"result_bytes"`
	MetadataSamples int64     `json:"metadata_samples"`
	Duration        Histogram `json:"duration"`
	Queue           Histogram `json:"queue"`
	Forward         Histogram `json:"forward"`
}
type MetricsRequest struct {
	Tool     string `json:"tool,omitempty"`
	Upstream string `json:"upstream,omitempty"`
	Since    string `json:"since,omitempty"`
	Until    string `json:"until,omitempty"`
}
type MetricsQuery struct {
	Tool, Upstream string
	Since, Until   time.Time
	Limit          int
}
type MetricsResponse struct {
	Groups    []MetricGroup `json:"groups"`
	Truncated bool          `json:"truncated"`
	Window    string        `json:"window"`
}
type MetricsSource interface {
	QueryToolCallMetrics(context.Context, MetricsQuery) ([]MetricGroup, error)
}
type QueryError struct{ Message string }

func (e *QueryError) Error() string { return e.Message }

// ReadMetrics queries only durable completed calls; starts never inflate counts.
// The result survives daemon restarts and is bounded by event retention.
func ReadMetrics(ctx context.Context, source MetricsSource, req MetricsRequest) (MetricsResponse, error) {
	q := MetricsQuery{Tool: req.Tool, Upstream: req.Upstream, Limit: MaxMetricGroups + 1}
	if len(req.Tool) > 256 || len(req.Upstream) > 256 {
		return MetricsResponse{}, &QueryError{"tool/upstream identifier too long"}
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
	groups, err := source.QueryToolCallMetrics(ctx, q)
	if err != nil {
		return MetricsResponse{}, err
	}
	out := MetricsResponse{Groups: groups, Window: "retained_events"}
	if len(out.Groups) > MaxMetricGroups {
		out.Groups = out.Groups[:MaxMetricGroups]
		out.Truncated = true
	}
	if out.Groups == nil {
		out.Groups = []MetricGroup{}
	}
	return out, nil
}

// Metrics exports process-lifetime completed-call observations. Identity and
// argument data are never labels. Durable introspection is a separate view.
type Metrics struct {
	calls                    metric.Int64Counter
	bytes                    metric.Int64Counter
	duration, queue, forward metric.Float64Histogram
}

func NewMetrics(meter metric.Meter) (*Metrics, error) {
	m := &Metrics{}
	var err error
	if m.calls, err = meter.Int64Counter("tether.tool.calls", metric.WithDescription("Completed tool calls, including rejected calls")); err != nil {
		return nil, err
	}
	if m.bytes, err = meter.Int64Counter("tether.tool.bytes", metric.WithUnit("By")); err != nil {
		return nil, err
	}
	bounds := make([]float64, len(HistogramBoundsMs))
	for i, b := range HistogramBoundsMs {
		bounds[i] = float64(b)
	}
	for _, h := range []struct {
		name string
		dst  *metric.Float64Histogram
	}{{"tether.tool.duration", &m.duration}, {"tether.tool.queue", &m.queue}, {"tether.tool.forward", &m.forward}} {
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
	labels := []attribute.KeyValue{attribute.String("tool", call.ToolName), attribute.String("upstream", call.Server), attribute.String("outcome", outcome)}
	opts := metric.WithAttributes(labels...)
	m.calls.Add(ctx, 1, opts)
	m.duration.Record(ctx, float64(call.DurationMs), opts)
	m.queue.Record(ctx, float64(call.QueueMs), opts)
	m.forward.Record(ctx, float64(call.ForwardMs), opts)
	m.bytes.Add(ctx, call.ArgsBytes, metric.WithAttributes(append(labels, attribute.String("direction", "arguments"))...))
	m.bytes.Add(ctx, call.ResultBytes, metric.WithAttributes(append(labels, attribute.String("direction", "result"))...))
}

var processMetrics, processMetricsError = NewMetrics(otel.Meter("tether/telemetry"))
