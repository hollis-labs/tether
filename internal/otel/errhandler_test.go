package otel

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestRateLimitedErrorHandlerThrottles(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))

	h := newRateLimitedErrorHandler(time.Minute)
	clock := time.Unix(1000, 0)
	h.now = func() time.Time { return clock }
	err := errors.New("dial tcp localhost:4318: connection refused")

	for i := 0; i < 100; i++ {
		h.Handle(err)
	}
	if n := strings.Count(buf.String(), "opentelemetry error"); n != 1 {
		t.Fatalf("want 1 log line within interval, got %d", n)
	}

	clock = clock.Add(2 * time.Minute)
	h.Handle(err)
	out := buf.String()
	if n := strings.Count(out, "opentelemetry error"); n != 2 {
		t.Fatalf("want 2 log lines after interval, got %d", n)
	}
	if !strings.Contains(out, "suppressed_since_last=99") {
		t.Fatalf("want suppressed count 99, got %q", out)
	}
}
