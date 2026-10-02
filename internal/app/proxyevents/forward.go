package proxyevents

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

// Forward subscribes after sinceSeq and forwards finished calls best-effort.
// The composition boundary supplies the existing credentialed daemon sink.
func Forward(ctx context.Context, bus events.Bus, sink func(context.Context, ProxyEventIngestRequest) error, sinceSeq int64) {
	ch, cancel, err := bus.Subscribe(ctx, events.Filter{SinceSeq: sinceSeq})
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("mcp: event forwarder failed to subscribe", "err", err)
		}
		return
	}
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Kind != events.EventTypeToolCallEnd {
				continue
			}

			// Unmarshal the ToolCallEvent payload.
			var tce events.ToolCallEvent
			if err := json.Unmarshal([]byte(ev.PayloadJSON), &tce); err != nil {
				slog.Warn("mcp: forwarder failed to unmarshal ToolCallEvent", "err", err)
				continue
			}

			body := ProxyEventIngestRequest{
				ClaimedSessionID: tce.ClaimedSessionID,
				SessionID:        tce.SessionID,
				Server:           tce.Server,
				ToolName:         tce.ToolName,
				ArgsSchemaFP:     tce.ArgsSchemaFP,
				DurationMs:       tce.DurationMs,
				OK:               tce.OK,
				Error:            TruncateProxyEventError(tce.Error),
				Timestamp:        tce.Timestamp.UTC().Format(time.RFC3339Nano),
			}
			postCtx, postCancel := context.WithTimeout(ctx, 3*time.Second)
			postErr := sink(postCtx, body)
			postCancel()
			if postErr != nil {
				slog.Debug("mcp: forwarder POST failed", "err", postErr)
			}

		}
	}
}
