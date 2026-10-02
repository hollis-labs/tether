package mcpadapter

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

type proxyEventWriter interface{ AppendProxyEvent(store.ProxyEvent) error }

// DaemonToolCallRecorder is a single bounded recorder shared by daemon views.
// It preserves the context resolved on the call, without HTTP self-observation,
// re-resolving a later binding, or blocking requests on a database commit.
type DaemonToolCallRecorder struct {
	queue    chan events.Event
	done     chan struct{}
	dropped  atomic.Uint64
	failures atomic.Uint64
}

func NewDaemonToolCallRecorder(ctx context.Context, bus events.Publisher, writer proxyEventWriter) *DaemonToolCallRecorder {
	r := &DaemonToolCallRecorder{queue: make(chan events.Event, daemonToolCallQueue), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-r.queue:
				var call events.ToolCallEvent
				if err := json.Unmarshal([]byte(event.PayloadJSON), &call); err != nil {
					r.failures.Add(1)
					continue
				}
				if event.Kind == events.EventTypeToolCallEnd && writer != nil {
					row := store.ProxyEvent{ToolCallDetails: call.ToolCallDetails, Attribution: call.Attribution, ClaimedSessionID: call.ClaimedSessionID, SessionID: call.SessionID, Server: call.Server, ToolName: call.ToolName, ArgsSchemaFP: call.ArgsSchemaFP, DurationMs: call.DurationMs, OK: call.OK, Error: api.TruncateProxyEventError(call.Error), Timestamp: call.Timestamp}
					if err := writer.AppendProxyEvent(row); err != nil {
						r.failures.Add(1)
					}
				}
				if bus != nil {
					if err := bus.Publish(ctx, event); err != nil {
						r.failures.Add(1)
					}
				}
			}
		}
	}()
	return r
}

func (r *DaemonToolCallRecorder) Publish(_ context.Context, event events.Event) error {
	if event.Kind != events.EventTypeToolCallStart && event.Kind != events.EventTypeToolCallEnd {
		return nil
	}
	// Timestamp is daemon-owned; attribution is the already resolved call context.
	var call events.ToolCallEvent
	if err := json.Unmarshal([]byte(event.PayloadJSON), &call); err != nil {
		return err
	}
	call.Timestamp = time.Now().UTC()
	raw, err := json.Marshal(call)
	if err != nil {
		return err
	}
	event.PayloadJSON = string(raw)
	select {
	case r.queue <- event:
	default:
		r.dropped.Add(1)
	}
	return nil
}

func (r *DaemonToolCallRecorder) Stats() (dropped, failures uint64) {
	return r.dropped.Load(), r.failures.Load()
}
