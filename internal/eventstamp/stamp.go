// Package eventstamp is Tether's stamp stage for the event stream (X4,
// CW-20261016-0139): it fills an event's scope dimensions, correlation and
// actor from verified context just before the event is written. A value is
// stamped only when something Tether itself resolved vouches for it; anything
// else is left empty. Nothing here reads a payload, a request argument or a
// message body, and nothing copies token material.
package eventstamp

import (
	"context"
	"sync/atomic"

	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"go.opentelemetry.io/otel/trace"
)

// Process names the environment the producing process belongs to. These are
// the same for every event the process emits. Each field is stamped only when
// set, so a process that does not know its environment yet stamps none of them.
type Process struct {
	EnvironmentID string
	Authority     string
	Host          string
}

// Stage is the stamp stage. The zero value is usable and stamps from context
// only; SetProcess adds the process dimensions once they are known (the bus is
// built before the environment identity is read).
type Stage struct {
	process atomic.Pointer[Process]
}

var (
	_ events.Stage  = (*Stage)(nil)
	_ events.TxSafe = (*Stage)(nil)
)

// New returns a stage with no process dimensions.
func New() *Stage { return &Stage{} }

// SetProcess installs the process dimensions. Safe to call while publishing.
func (s *Stage) SetProcess(p Process) { s.process.Store(&p) }

// TxSafe reports that Stamp never touches the database.
func (*Stage) TxSafe() bool { return true }

// Stamp fills what ctx and the stage's process vouch for and leaves anything
// the producer already set alone.
func (s *Stage) Stamp(ctx context.Context, e *events.Event) error {
	if p := s.process.Load(); p != nil {
		set(&e.Envelope.Dims.EnvironmentID, p.EnvironmentID)
		set(&e.Envelope.Dims.Authority, p.Authority)
		set(&e.Envelope.Dims.Host, p.Host)
	}
	stampSession(ctx, e)
	stampActor(ctx, e)
	stampCorr(ctx, e)
	return nil
}

func set(dst *string, v string) {
	if *dst == "" {
		*dst = v
	}
}

// stampSession fills the session-scoped dimensions from the caller's resolved
// session, and only for an event about that same session: the snapshot
// describes the caller, so it says nothing about some other session's event.
// An unresolved or unmanaged caller contributes nothing (rule 3).
func stampSession(ctx context.Context, e *events.Event) {
	snap, ok := callcontext.FromContext(ctx)
	if !ok || !snap.SessionResolved || snap.SessionID == "" || e.SessionID != snap.SessionID {
		return
	}
	d := &e.Envelope.Dims
	set(&d.AgentURN, snap.AgentURN)
	set(&d.WorkstreamID, snap.WorkstreamID)
	set(&d.TaskID, snap.TaskID)
	set(&d.LaunchID, snap.LaunchID)
	set(&d.ProjectID, snap.ProjectID)
	set(&d.LogicalAgentID, snap.LogicalAgentID)
}

// stampActor says who caused the event, from the verified principal in ctx.
// Only two callers map to a mesh actor today: the real local operator, and a
// session (credential or issued session-bound token) whose agent address the
// registry binding confirmed. An operator-minted narrow token carries the
// operator's id but is not the operator, so it gets no actor. A claimed
// session or any unverified caller gets none either: "claimed" cannot name an
// actor URN without guessing one.
func stampActor(ctx context.Context, e *events.Event) {
	if e.Envelope.Actor != (events.Actor{}) {
		return
	}
	p, ok := identity.FromContext(ctx)
	if !ok || !p.Verified {
		return
	}
	switch {
	case identity.IsOperatorCredential(p):
		e.Envelope.Actor = events.Actor{Kind: "user", URN: identity.OperatorID, Verified: "token"}
	case p.SessionID != "" && (p.Kind == "session" || identity.IsBoundAgentToken(p)):
		snap, ok := callcontext.FromContext(ctx)
		if ok && snap.SessionResolved && snap.SessionID == p.SessionID && snap.AgentURN != "" {
			e.Envelope.Actor = events.Actor{Kind: "agent", URN: snap.AgentURN, Verified: "token"}
		}
	}
}

// stampCorr copies the W3C trace identity of the current span. These are
// identifiers minted by the tracer, never request data.
func stampCorr(ctx context.Context, e *events.Event) {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return
	}
	set(&e.Envelope.Corr.TraceID, sc.TraceID().String())
	set(&e.Envelope.Corr.SpanID, sc.SpanID().String())
}
