package proxyevents

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

// Forward follows the bus after sinceSeq and forwards finished calls
// best-effort. The composition boundary supplies the existing credentialed
// daemon sink. It resumes from its last cursor if the bus closes its channel
// (an evicted slow consumer) instead of ending, and never sees stream.gap
// frames (events.Follow logs them). It returns when ctx is done.
func Forward(ctx context.Context, bus events.Bus, sink func(context.Context, ProxyEventIngestRequest) error, sinceSeq int64) {
	events.Follow(ctx, bus, events.Filter{SinceSeq: sinceSeq}, events.FollowOptions{Name: "mcp: event forwarder"}, func(ev events.Event) {
		if ev.Kind != events.EventTypeToolCallEnd {
			return
		}

		// Unmarshal the ToolCallEvent payload.
		var tce events.ToolCallEvent
		if err := json.Unmarshal([]byte(ev.PayloadJSON), &tce); err != nil {
			slog.Warn("mcp: forwarder failed to unmarshal ToolCallEvent", "err", err)
			return
		}

		body := IngestCall(tce, ProxyEventPhaseEnd, false)
		postCtx, postCancel := context.WithTimeout(ctx, 3*time.Second)
		postErr := sink(postCtx, body)
		postCancel()
		if postErr != nil {
			slog.Debug("mcp: forwarder POST failed", "err", postErr)
		}
	})
}
