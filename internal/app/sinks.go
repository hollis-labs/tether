package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hollis-labs/go-agent-sessions/agentsessions"

	"github.com/chrispian/agent-mux/internal/events"
	"github.com/chrispian/agent-mux/internal/store"
)

// stateSinkAdapter adapts *store.Store to agentsessions.StateSink. Lib's
// typed State (launching/running/done/failed) maps 1:1 to mux's string-
// based vocabulary; the only translation today is the lib's Done +
// Reason="killed" → mux's "killed". That happens in eventSinkAdapter
// (where Reason is observed), not here — by the time the StateSink fires,
// the lib has already collapsed killed → done. Mux's mux-domain Killed
// state is recoverable from the LifecycleEvent payload, not from the
// state column.
type stateSinkAdapter struct {
	db *store.Store
}

func (a stateSinkAdapter) UpdateSessionState(id string, state agentsessions.State, pid int, exit *int) error {
	return a.db.UpdateSessionState(id, string(state), pid, exit)
}

// attachmentSinkAdapter adapts *store.Store to agentsessions.AttachmentSink.
// Translates time.Time → RFC3339 string (the format mux's store schema uses).
type attachmentSinkAdapter struct {
	db *store.Store
}

func (a attachmentSinkAdapter) CreateClientAttachment(attachID, sessionID, clientKind string, attachedAt time.Time) error {
	return a.db.CreateClientAttachment(attachID, sessionID, clientKind, attachedAt.UTC().Format(time.RFC3339))
}

func (a attachmentSinkAdapter) DetachClientAttachment(attachID string, detachedAt time.Time) error {
	return a.db.DetachClientAttachment(attachID, detachedAt.UTC().Format(time.RFC3339))
}

// eventSinkAdapter adapts an events.Publisher to agentsessions.EventSink.
//
// The lib's LifecycleEvent does not carry logical_agent_id (sessions are
// process-level; logical-agent identity is mux-domain). The adapter
// resolves it on each Emit by looking up the session's metadata in the
// owning Manager. mgr is set after Manager construction via SetManager —
// the circular reference (sink referenced by Manager, Manager referenced
// by sink) is resolved post-hoc.
//
// State transitions also re-derive a mux-domain "from"/"to" pair: the lib
// emits Done with Reason="killed" for explicitly-stopped sessions; mux
// persists this as Killed on the session row. Other states map 1:1.
type eventSinkAdapter struct {
	bus events.Publisher
	mgr *agentsessions.Manager // set post-construction; nil-safe
}

// SetManager wires the lookup target after Manager construction. Safe to
// call exactly once during composition; the Emit path reads mgr without
// synchronization (set-once-before-first-event ordering).
func (a *eventSinkAdapter) SetManager(m *agentsessions.Manager) { a.mgr = m }

// sessionStateChangedPayload mirrors the v0.0.4 mux payload shape so
// existing consumers of `session.state_changed` envelopes keep working
// without contract surgery.
type sessionStateChangedPayload struct {
	From     string `json:"from"`
	To       string `json:"to"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

func (a *eventSinkAdapter) Emit(ctx context.Context, ev agentsessions.LifecycleEvent) {
	if a.bus == nil {
		return
	}
	from, to := mapLifecycleStates(ev)
	payload, err := json.Marshal(sessionStateChangedPayload{
		From:     from,
		To:       to,
		ExitCode: ev.ExitCode,
		Reason:   ev.Reason,
	})
	if err != nil {
		return
	}

	var logicalAgentID string
	if a.mgr != nil {
		if info, ok := a.mgr.Get(ev.SessionID); ok {
			logicalAgentID = info.Meta["logical_agent_id"]
		}
	}

	_ = a.bus.Publish(ctx, events.Event{
		Scope:          events.ScopeSession,
		SessionID:      ev.SessionID,
		LogicalAgentID: logicalAgentID,
		Kind:           events.KindSessionStateChanged,
		PayloadJSON:    string(payload),
	})
}

// mapLifecycleStates translates a lib LifecycleEvent's From/To pair into
// mux's string state vocabulary. The only divergence is Done+killed →
// "killed"; everything else passes through unchanged.
func mapLifecycleStates(ev agentsessions.LifecycleEvent) (from, to string) {
	from = string(ev.From)
	to = string(ev.To)
	if ev.To == agentsessions.StateDone && ev.Reason == "killed" {
		to = "killed"
	}
	return from, to
}

// Static interface checks. The assertions let the linter see the types
// as "used" and surface contract drift at build time if the lib's sink
// shapes change.
var (
	_ agentsessions.StateSink      = stateSinkAdapter{}
	_ agentsessions.AttachmentSink = attachmentSinkAdapter{}
	_ agentsessions.EventSink      = (*eventSinkAdapter)(nil)
)
