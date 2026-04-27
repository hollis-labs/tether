package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hollis-labs/go-agent-sessions/agentsessions"

	"github.com/chrispian/agent-mux/internal/events"
	"github.com/chrispian/agent-mux/internal/store"
)

// stateSinkAdapter adapts *store.Store to agentsessions.StateSink. The
// lib's vocabulary (launching/running/done/failed) does not match mux's
// persisted vocabulary (launching/running/completed/failed/killed)
// exactly: lib terminal "done" persists as mux's "completed".
//
// The StateSink callback does not receive the lib's stop reason, so this
// path cannot distinguish an explicit kill from a clean completion at
// the column level — both land as "completed". The killed-vs-completed
// distinction is preserved in the events stream (eventSinkAdapter sees
// Reason and emits to="killed" in the session.state_changed payload).
// Consumers that need DB-level kill detection can join against the
// events table; the column itself is best-effort.
type stateSinkAdapter struct {
	db *store.Store
}

// muxSessionState normalises a lib State value to mux's persisted
// vocabulary. Lib "done" → mux "completed"; everything else passthrough.
// "killed" is not a lib state today — it's recovered from LifecycleEvent
// .Reason in eventSinkAdapter / mapLifecycleStates.
func muxSessionState(state agentsessions.State) string {
	if state == agentsessions.StateDone {
		return "completed"
	}
	return string(state)
}

func (a stateSinkAdapter) UpdateSessionState(id string, state agentsessions.State, pid int, exit *int) error {
	return a.db.UpdateSessionState(id, muxSessionState(state), pid, exit)
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
// mux's string state vocabulary used in session.state_changed payloads.
// Base translation via muxSessionState (Done → completed); when To is
// Done with Reason="killed", override to "killed" so consumers reading
// the events stream can distinguish kill from clean completion (a
// distinction the StateSink-driven sessions.state column cannot
// preserve).
func mapLifecycleStates(ev agentsessions.LifecycleEvent) (from, to string) {
	from = muxSessionState(ev.From)
	to = muxSessionState(ev.To)
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
