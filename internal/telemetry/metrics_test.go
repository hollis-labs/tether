package telemetry

import (
	"context"
	"github.com/hollis-labs/tether/internal/events"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"testing"
)

func TestOTelMetricsCaptureDeniedAndSuccessfulCalls(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	metrics, err := NewMetrics(provider.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []events.ToolCallEvent{{ToolName: "read", Server: "alpha", OK: true, DurationMs: 26, ToolCallDetails: events.ToolCallDetails{ArgsBytes: 10, ResultBytes: 20, GatewayMs: 1, ForwardMs: 25}}, {ToolName: "read", Server: "alpha", ToolCallDetails: events.ToolCallDetails{ErrorClass: events.ToolErrorDenied}, DurationMs: 3}} {
		metrics.Observe(context.Background(), call)
	}
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	var calls int64
	var histogramCount uint64
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			switch values := metric.Data.(type) {
			case metricdata.Sum[int64]:
				if metric.Name == "tether.tool.calls" {
					for _, point := range values.DataPoints {
						calls += point.Value
						for _, label := range point.Attributes.ToSlice() {
							if label.Key == "principal_id" || label.Key == "arguments" {
								t.Fatal("sensitive metric label")
							}
						}
					}
				}
			case metricdata.Histogram[float64]:
				if metric.Name == "tether.tool.duration" {
					for _, point := range values.DataPoints {
						histogramCount += point.Count
						if len(point.Bounds) != len(HistogramBoundsMs) {
							t.Fatal("different buckets")
						}
					}
				}
			}
		}
	}
	if calls != 2 || histogramCount != 2 {
		t.Fatalf("calls %d histogram %d", calls, histogramCount)
	}
}
