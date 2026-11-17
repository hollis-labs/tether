package mcpadapter

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/catalog"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/eventstamp"
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
				// Only the canonical pair reaches the event log (CW-20261113-0009).
				// The legacy tool_call_start/tool_call_end pair duplicated every
				// call there; its full payload lives on in proxy_events above.
				if bus != nil && isCanonicalToolCall(event.Kind) {
					if err := bus.Publish(ctx, event); err != nil {
						r.failures.Add(1)
					}
				}
			}
		}
	}()
	return r
}

func (r *DaemonToolCallRecorder) Publish(ctx context.Context, event events.Event) error {
	canonical := isCanonicalToolCall(event.Kind)
	// A legacy start has no proxy_events row and no longer goes to the event
	// log; tool.call.started carries it (CW-20261113-0009).
	if !canonical && event.Kind != events.EventTypeToolCallEnd {
		return nil
	}
	// Stamp caller-owned verified dimensions before crossing the async queue.
	// The bus later adds process dimensions with its configured stamp stage.
	if err := eventstamp.New().Stamp(ctx, &event); err != nil {
		return err
	}
	if !canonical {
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
	}
	select {
	case r.queue <- event:
	default:
		r.dropped.Add(1)
	}
	return nil
}

func isCanonicalToolCall(kind string) bool {
	return kind == catalog.KindToolCallStarted || kind == catalog.KindToolCallFinished
}

func (r *DaemonToolCallRecorder) Stats() (dropped, failures uint64) {
	return r.dropped.Load(), r.failures.Load()
}
