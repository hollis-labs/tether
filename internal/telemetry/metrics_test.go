package telemetry

import (
	"context"
	"fmt"
	"testing"

	"github.com/hollis-labs/tether/internal/events"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
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
				for _, point := range values.DataPoints {
					assertMetricLabels(t, point.Attributes)
				}
				if metric.Name == "tether.tool.calls" {
					for _, point := range values.DataPoints {
						calls += point.Value
					}
				}
			case metricdata.Histogram[float64]:
				for _, point := range values.DataPoints {
					assertMetricLabels(t, point.Attributes)
				}
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

func assertMetricLabels(t *testing.T, labels attribute.Set) {
	t.Helper()
	want := map[attribute.Key]bool{"tool": true, "upstream": true, "outcome": true}
	if labels.Len() != len(want) {
		t.Fatalf("metric labels = %v, want keys %v", labels, want)
	}
	for _, label := range labels.ToSlice() {
		if !want[label.Key] {
			t.Fatalf("unexpected metric label %q", label.Key)
		}
	}
}

func TestOTelMetricSeriesAreBoundedWithoutLosingCallCounts(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	// Disable SDK cardinality limiting so this verifies our own bound.
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithCardinalityLimit(0))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	metrics, err := NewMetrics(provider.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	const total = MaxOTelCallSeries * 3
	for i := range total {
		metrics.Observe(context.Background(), events.ToolCallEvent{ToolName: fmt.Sprintf("tool_%d", i), Server: fmt.Sprintf("upstream_%d", i), OK: true})
	}
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name == "tether.tool.calls" {
				points := metric.Data.(metricdata.Sum[int64]).DataPoints
				if len(points) > MaxOTelCallSeries+1 {
					t.Fatalf("unbounded series: %d", len(points))
				}
				var calls int64
				for _, point := range points {
					calls += point.Value
					assertMetricLabels(t, point.Attributes)
				}
				if calls != total {
					t.Fatalf("overflow lost calls: %d / %d", calls, total)
				}
				return
			}
		}
	}
	t.Fatal("missing call counter")
}

func TestDeniedJunkNamesCannotStarveLegitimateMetricSeries(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithCardinalityLimit(0))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	metrics, err := NewMetrics(provider.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2000 {
		metrics.Observe(context.Background(), events.ToolCallEvent{ToolName: fmt.Sprintf("junk_%d", i), ToolCallDetails: events.ToolCallDetails{ErrorClass: events.ToolErrorDenied}})
	}
	metrics.Observe(context.Background(), events.ToolCallEvent{ToolName: "tether_docs_list", Server: "tether", OK: true})
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != "tether.tool.calls" {
				continue
			}
			points := metric.Data.(metricdata.Sum[int64]).DataPoints
			var calls int64
			var realTool bool
			for _, point := range points {
				assertMetricLabels(t, point.Attributes)
				calls += point.Value
				tool, _ := point.Attributes.Value("tool")
				outcome, _ := point.Attributes.Value("outcome")
				if tool.AsString() == "tether_docs_list" && outcome.AsString() == "ok" && point.Value == 1 {
					realTool = true
				}
			}
			if !realTool || calls != 2001 || len(points) > MaxOTelDeniedSeries+2 {
				t.Fatalf("real tool=%v calls=%d series=%d", realTool, calls, len(points))
			}
			return
		}
	}
	t.Fatal("missing call counter")
}
