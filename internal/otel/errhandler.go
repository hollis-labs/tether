package otel

import (
	"log/slog"
	"sync"
	"time"

	sdkotel "go.opentelemetry.io/otel"
)

// errorLogInterval bounds how often identical exporter failures are logged.
// With no collector listening on the default localhost:4318, every export
// attempt fails; unthrottled that grew stderr.log to hundreds of MB.
const errorLogInterval = 10 * time.Minute

// rateLimitedErrorHandler logs the first error immediately, then at most one
// line per interval, reporting how many were suppressed in between.
type rateLimitedErrorHandler struct {
	mu         sync.Mutex
	interval   time.Duration
	now        func() time.Time
	last       time.Time
	suppressed int
}

func newRateLimitedErrorHandler(interval time.Duration) *rateLimitedErrorHandler {
	return &rateLimitedErrorHandler{interval: interval, now: time.Now}
}

// Handle implements otel.ErrorHandler.
func (h *rateLimitedErrorHandler) Handle(err error) {
	if err == nil {
		return
	}
	h.mu.Lock()
	now := h.now()
	if !h.last.IsZero() && now.Sub(h.last) < h.interval {
		h.suppressed++
		h.mu.Unlock()
		return
	}
	suppressed := h.suppressed
	h.last, h.suppressed = now, 0
	h.mu.Unlock()
	slog.Warn("opentelemetry error", "error", err, "suppressed_since_last", suppressed, "log_interval", h.interval.String())
}

func installErrorHandler() {
	sdkotel.SetErrorHandler(newRateLimitedErrorHandler(errorLogInterval))
}
