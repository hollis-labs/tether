// Package telemetry owns secret-free tool-call observations and event recording.
// Transports supply decoded sizes and typed outcomes, never argument values.
package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/redact"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

type Service struct {
	Publisher events.Publisher
	Secrets   *redact.Set
}
type Call struct {
	Name, Server, SessionID, ClaimedSessionID, Fingerprint, Profile, Mode string
	ArgsBytes                                                             int64
}
type Outcome struct {
	OK          bool
	Error       string
	Class       events.ToolErrorClass
	ResultBytes int64
}
type observationKey struct{}

// Observation is private per-call state, safe for asynchronous upstream callbacks.
type Observation struct {
	mu      sync.Mutex
	event   events.ToolCallEvent
	started time.Time
	forward time.Duration
	class   events.ToolErrorClass
	span    trace.Span
}

func (s *Service) Start(ctx context.Context, call Call) (context.Context, *Observation) {
	ctx, span := otel.Tracer("tether/telemetry").Start(ctx, "tether.tool.call")
	now := time.Now()
	attribution, _ := callcontext.FromContext(ctx)
	if !attribution.Verified {
		attribution = callcontext.Snapshot{}
	}
	sc := span.SpanContext()
	details := events.ToolCallDetails{Profile: call.Profile, DiscoveryMode: call.Mode, ArgsBytes: call.ArgsBytes}
	if sc.IsValid() {
		details.TraceID = sc.TraceID().String()
		details.SpanID = sc.SpanID().String()
	}
	o := &Observation{started: now, span: span, event: events.ToolCallEvent{ToolCallDetails: details, Attribution: attribution, SessionID: call.SessionID, ClaimedSessionID: call.ClaimedSessionID, ToolName: call.Name, Server: call.Server, ArgsSchemaFP: call.Fingerprint, Timestamp: now}}
	s.publish(ctx, events.EventTypeToolCallStart, o.event)
	return context.WithValue(ctx, observationKey{}, o), o
}

// SetErrorClass records a typed boundary decision without parsing human error text.
func SetErrorClass(ctx context.Context, class events.ToolErrorClass) {
	if o, ok := ctx.Value(observationKey{}).(*Observation); ok {
		o.mu.Lock()
		o.class = class
		o.mu.Unlock()
	}
}

// Forward measures only the upstream/native dispatch, excluding policy and setup.
func Forward(ctx context.Context) func() {
	o, ok := ctx.Value(observationKey{}).(*Observation)
	if !ok {
		return func() {}
	}
	start := time.Now()
	return func() { o.mu.Lock(); o.forward += time.Since(start); o.mu.Unlock() }
}
func (s *Service) End(ctx context.Context, o *Observation, out Outcome) events.ToolCallEvent {
	o.mu.Lock()
	ev := o.event
	duration := time.Since(o.started)
	ev.DurationMs = duration.Milliseconds()
	ev.ForwardMs = o.forward.Milliseconds()
	ev.GatewayMs = (duration - o.forward).Milliseconds()
	if ev.GatewayMs < 0 {
		ev.GatewayMs = 0
	}
	ev.ResultBytes = out.ResultBytes
	ev.OK = out.OK
	ev.Timestamp = time.Now()
	ev.ErrorClass = o.class
	o.mu.Unlock()
	if !out.OK {
		if ev.ErrorClass == "" {
			ev.ErrorClass = out.Class
		}
		if ev.ErrorClass == "" {
			ev.ErrorClass = events.ToolErrorUpstream
		}
		ev.Error = s.Secrets.Redact(out.Error)
		if len(ev.Error) > events.MaxToolCallErrorBytes {
			ev.ErrorTruncated = true
			ev.Error = events.TruncateToolCallError(ev.Error)
		}
	} else {
		ev.ErrorClass = ""
	}
	s.publish(ctx, events.EventTypeToolCallEnd, ev)
	o.span.End()
	return ev
}
func (s *Service) publish(ctx context.Context, kind string, ev events.ToolCallEvent) {
	if s.Publisher == nil {
		return
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		slog.Warn("telemetry: encode call", "err", err)
		return
	}
	scope := events.ScopeSession
	if ev.SessionID == "" {
		scope = events.ScopeDaemon
	}
	// Recording failures must not change the result of the observed call.
	if err = s.Publisher.Publish(context.WithoutCancel(ctx), events.Event{Scope: scope, SessionID: ev.SessionID, Kind: kind, PayloadJSON: string(raw)}); err != nil {
		slog.Warn("telemetry: publish call", "err", err)
	}
}

// ErrorClass classifies cancellation and timeout by their typed cause.
func ErrorClass(err error) events.ToolErrorClass {
	if errors.Is(err, context.DeadlineExceeded) {
		return events.ToolErrorTimeout
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return events.ToolErrorTimeout
	}
	return events.ToolErrorUpstream
}

// IsObserved reports whether the outer call recorder already owns the trace.
func IsObserved(ctx context.Context) bool {
	_, ok := ctx.Value(observationKey{}).(*Observation)
	return ok
}
