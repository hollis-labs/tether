package agentevents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
)

// Tier is how directly the vendor reported what the event says. It maps one to one onto runtimeevents.Confidence.
type Tier string

const (
	// Exact: the vendor reported this event itself (a hook firing, a JSON-RPC notification).
	Exact Tier = "exact"
	// Derived: reconstructed from a structured but indirect source (a status file, a transcript line).
	Derived Tier = "derived"
	// Inferred: produced by a heuristic (a mtime heartbeat, a text match). Never authoritative on its own.
	Inferred Tier = "inferred"
)

func (t Tier) confidence() (runtimeevents.Confidence, error) {
	switch t {
	case Exact:
		return runtimeevents.ConfidenceExact, nil
	case Derived:
		return runtimeevents.ConfidenceDerived, nil
	case Inferred:
		return runtimeevents.ConfidenceInferred, nil
	}
	return "", fmt.Errorf("agentevents: unknown tier %q", string(t))
}

// SessionURN is the subject of every event of a Tether session. It is the same form the token issuer uses for a session
// principal (msg://session/local/<id>).
func SessionURN(sessionID string) string { return "msg://session/local/" + sessionID }

// Observation is one thing an adapter saw, before the envelope exists.
type Observation struct {
	Kind runtimeevents.EventKind
	// Channel is the transport the adapter read it from (runtimeevents.ChannelHook, ChannelJSONRPC ...).
	Channel runtimeevents.SourceChannel
	Tier    Tier
	// Payload is marshaled as JSON; nil is an absent payload. Keep it to the catalog's payload schema for the kind.
	Payload any
	// Time is when the vendor says it happened; zero means now.
	Time   time.Time
	TurnID string
	// ParentID links to an earlier event of this producer that this one answers (a tool result to its tool use).
	ParentID string
	// CausationID names the event, from any producer, that caused this one (a subagent spawn to the tool use behind it).
	CausationID string
	// Actor is who did it, when the adapter can tell. Both fields are required together.
	ActorURN, ActorKind, ActorRole string
}

// Recorder stamps and stores the events of one session. It is safe for concurrent use.
type Recorder struct {
	// App names the producer in the envelope ("tether").
	App string
	// SessionID is the Tether session id. Required.
	SessionID string
	// Process describes the agent process: provider ("claude", "codex", "antigravity"), runtime, pid, provider session id.
	Process runtimeevents.Process
	// Sink receives the finished event. Required. A [Spool] is the usual one.
	Sink Sink
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Sink stores one finished event. A Sink assigns Sequence when it is zero.
type Sink interface {
	Append(ctx context.Context, ev runtimeevents.Event) (runtimeevents.Event, error)
}

// ErrInvalid is returned for an observation that cannot become a valid event.
var ErrInvalid = errors.New("agentevents: invalid observation")

// Record builds the event for o, validates it against the v1.1 envelope rules, and stores it. It returns the stored event.
// A failure is the caller's to log; an adapter must never let it stop the agent.
func (r *Recorder) Record(ctx context.Context, o Observation) (runtimeevents.Event, error) {
	if r == nil || r.Sink == nil || r.SessionID == "" {
		return runtimeevents.Event{}, fmt.Errorf("%w: recorder needs a sink and a session id", ErrInvalid)
	}
	if o.Kind == "" || o.Channel == "" {
		return runtimeevents.Event{}, fmt.Errorf("%w: kind and channel are required", ErrInvalid)
	}
	conf, err := o.Tier.confidence()
	if err != nil {
		return runtimeevents.Event{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	at := o.Time
	if at.IsZero() {
		at = now()
	}
	ev := runtimeevents.Event{
		SchemaVersion: runtimeevents.SchemaVersion,
		ID:            runtimeevents.NewEventID(),
		Kind:          o.Kind,
		Time:          at.UTC(),
		App:           r.App,
		SessionID:     r.SessionID,
		Subject:       SessionURN(r.SessionID),
		TurnID:        o.TurnID,
		ParentID:      o.ParentID,
		Process:       r.Process,
		Source:        runtimeevents.Source{Channel: o.Channel, Confidence: conf},
	}
	if o.Payload != nil {
		raw, err := marshal(o.Payload)
		if err != nil {
			return runtimeevents.Event{}, fmt.Errorf("%w: payload: %w", ErrInvalid, err)
		}
		ev.Payload = raw
	}
	if o.CausationID != "" {
		ev.Corr = &runtimeevents.Corr{CausationID: o.CausationID}
	}
	if o.ActorURN != "" || o.ActorKind != "" {
		ev.Actor = &runtimeevents.Actor{URN: o.ActorURN, Kind: o.ActorKind, Role: o.ActorRole}
	}
	// The sink assigns the sequence; validate with a stand-in so a missing sequence is not mistaken for a bad event.
	check := ev
	check.Sequence = 1
	if err := check.Validate(); err != nil {
		return runtimeevents.Event{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return r.Sink.Append(ctx, ev)
}

func marshal(v any) ([]byte, error) {
	switch p := v.(type) {
	case []byte:
		return p, nil
	case string:
		return nil, fmt.Errorf("payload must be a JSON value, not a bare string %q", strings.TrimSpace(p))
	}
	return jsonMarshal(v)
}
