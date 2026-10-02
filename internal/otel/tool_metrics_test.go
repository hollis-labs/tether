package otel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/telemetry"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// Exercise the production initializer and pre-initialization instruments,
// rather than just registering instruments against an isolated test provider.
func TestInitExportsCompletedToolCalls(t *testing.T) {
	restoreLogging(t)
	previousMeter, previousTracer := otel.GetMeterProvider(), otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetMeterProvider(previousMeter)
		otel.SetTracerProvider(previousTracer)
		otel.SetTextMapPropagator(previousPropagator)
	})
	requests := make(chan *collectormetrics.ExportMetricsServiceRequest, 4)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/metrics" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			request := &collectormetrics.ExportMetricsServiceRequest{}
			if err := proto.Unmarshal(body, request); err != nil {
				t.Error(err)
			}
			requests <- request
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	t.Setenv(disabledEnv, "")
	t.Setenv(legacyEnv, "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", strings.TrimPrefix(collector.URL, "http://"))
	shutdown, err := Init(context.Background(), "tether-test", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	service := &telemetry.Service{}
	for _, outcome := range []telemetry.Outcome{{OK: true}, {Class: events.ToolErrorDenied}} {
		ctx, observation := service.Start(context.Background(), telemetry.Call{Name: "read", Server: "alpha"})
		service.End(ctx, observation, outcome)
	}
	provider, ok := otel.GetMeterProvider().(*sdkmetric.MeterProvider)
	if !ok {
		t.Fatal("initializer did not install metric provider")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := provider.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-requests:
		var calls int64
		for _, resource := range request.ResourceMetrics {
			for _, scope := range resource.ScopeMetrics {
				for _, metric := range scope.Metrics {
					if metric.Name == "tether.tool.calls" {
						for _, point := range metric.GetSum().DataPoints {
							calls += point.GetAsInt()
						}
					}
				}
			}
		}
		if calls != 2 {
			t.Fatalf("exported calls = %d, want success + denial", calls)
		}
	case <-ctx.Done():
		t.Fatal("metric export did not reach local collector")
	}
}
