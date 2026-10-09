package app

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	gop "github.com/hollis-labs/substrate/harness/adapters/provider"
	gopevents "github.com/hollis-labs/substrate/harness/adapters/provider/events"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

// stopRequests records which sessions a caller asked to stop, so their
// terminal transition is recorded as "killed" rather than "completed"
// (CW-20260930-0250). The lib knows a session is being stopped (its
// entry.killing flag) but lands it as StateDone and passes neither the
// flag nor a reason to the StateSink or the EventSink, so tether keeps
// its own record. Service.StopSession marks a session before signaling
// it and clears the mark once the terminal state has been written.
//
// Marks are counted, one clear per mark, so a second stop of the same
// session that finds it already gone cannot drop the first stop's mark
// before the terminal state is written.
//
// Methods are nil-safe: a composition without a stopRequests (tests that
// build a Manager by hand) records stops the old way, as "completed".
type stopRequests struct {
	mu      sync.Mutex
	ids     map[string]int
	reasons map[string]string
}

func (r *stopRequests) mark(id string) {
	r.markWithReason(id, "")
}

// markWithReason is mark that also names why the session is being stopped.
// The reason becomes the `reason` of its terminal session.state_changed
// event, as ShutdownStopReason does for a planned daemon shutdown. An empty
// reason leaves any earlier one in place.
func (r *stopRequests) markWithReason(id, reason string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ids == nil {
		r.ids = make(map[string]int)
	}
	r.ids[id]++
	if reason != "" {
		if r.reasons == nil {
			r.reasons = make(map[string]string)
		}
		r.reasons[id] = reason
	}
}

func (r *stopRequests) clear(id string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ids[id] <= 1 {
		delete(r.ids, id)
		delete(r.reasons, id)
		return
	}
	r.ids[id]--
}

// reason returns the reason a pending stop of id was given, or "".
func (r *stopRequests) reason(id string) string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reasons[id]
}

func (r *stopRequests) requested(id string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ids[id] > 0
}

// stateSinkAdapter adapts *store.Store to agentsessions.StateSink. The
// lib's vocabulary (launching/running/done/failed) does not match tether's
// persisted vocabulary (launching/running/completed/failed/killed)
// exactly: lib terminal "done" persists as tether's "completed", and a
// terminal state reached after a stop request persists as "killed".
type stateSinkAdapter struct {
	db    *store.Store
	stops *stopRequests
}

// tetherSessionState normalises a lib State value to tether's persisted
// vocabulary. Lib "done" → tether "completed"; everything else passthrough.
// "killed" is not a lib state — terminalState derives it from a stop
// request.
func tetherSessionState(state agentsessions.State) string {
	if state == agentsessions.StateDone {
		return "completed"
	}
	return string(state)
}

// terminalState maps a lib state to tether's vocabulary for one session:
// any terminal state (done or failed) reached after a stop request is
// "killed", whatever the exit code. A stopped process may exit 0 (it
// caught SIGTERM and exited cleanly) or -1 (a signal ended it), so the
// exit code alone cannot tell a stop from a finish.
func terminalState(state agentsessions.State, stopRequested bool) string {
	if stopRequested && (state == agentsessions.StateDone || state == agentsessions.StateFailed) {
		return string(session.StateKilled)
	}
	return tetherSessionState(state)
}

func (a stateSinkAdapter) UpdateSessionState(id string, state agentsessions.State, pid int, exit *int) error {
	if shimBridgeTerminal(a.db, id, state) {
		// The bridge ending does not establish provider exit. Its watcher settles
		// the canonical host outcome; Stop commits its own guarded outcome.
		return nil
	}
	if err := a.db.UpdateSessionState(id, terminalState(state, a.stops.requested(id)), pid, exit); err != nil {
		return err
	}
	if state == agentsessions.StateRunning && pid > 0 {
		recordProcessStart(a.db, id, pid)
	}
	return nil
}

// recordProcessStart stores when the session's process started, so the next
// daemon's startup sweep can tell it from a later process that reuses its
// pid (CW-20260912-0085). Best effort: without it the sweep falls back to a
// weaker check.
func recordProcessStart(db *store.Store, id string, pid int) {
	started, ok := osProcessInspector{}.startTime(pid)
	if !ok {
		return
	}
	if err := db.SetSessionProcessStart(id, pid, started); err != nil {
		log.Printf("app: record process start for session %s (pid %d) failed: %v", id, pid, err)
	}
}

// attachmentSinkAdapter adapts *store.Store to agentsessions.AttachmentSink.
// Translates time.Time → RFC3339 string (the format tether's store schema uses).
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
// process-level; logical-agent identity is tether-domain). The adapter
// resolves it on each Emit by looking up the session's metadata in the
// owning Manager. mgr is set after Manager construction via SetManager —
// the circular reference (sink referenced by Manager, Manager referenced
// by sink) is resolved post-hoc.
//
// State transitions also re-derive a tether-domain "from"/"to" pair: a
// terminal transition after a stop request is "killed", matching what
// stateSinkAdapter persists on the session row. Other states map 1:1.
type eventSinkAdapter struct {
	db    *store.Store
	bus   events.Publisher
	mgr   *agentsessions.Manager // set post-construction; nil-safe
	stops *stopRequests
}

// SetManager wires the lookup target after Manager construction. Safe to
// call exactly once during composition; the Emit path reads mgr without
// synchronization (set-once-before-first-event ordering).
func (a *eventSinkAdapter) SetManager(m *agentsessions.Manager) { a.mgr = m }

// sessionStateChangedPayload mirrors the v0.0.4 tether payload shape so
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
	from, to := mapLifecycleStates(ev, a.stops.requested(ev.SessionID))
	reason := ev.Reason
	if shimBridgeTerminal(a.db, ev.SessionID, ev.To) {
		return
	}
	if to == string(session.StateKilled) {
		if r := a.stops.reason(ev.SessionID); r != "" {
			reason = r
		}
	}
	payload, err := json.Marshal(sessionStateChangedPayload{
		From:     from,
		To:       to,
		ExitCode: ev.ExitCode,
		Reason:   reason,
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
// tether's string state vocabulary used in session.state_changed payloads.
// To goes through terminalState, so a stopped session's event says
// "killed", as its session row does. Done with Reason="killed" is also
// read as a stop, for a lib that reports one that way.
func mapLifecycleStates(ev agentsessions.LifecycleEvent, stopRequested bool) (from, to string) {
	from = tetherSessionState(ev.From)
	killed := stopRequested || (ev.To == agentsessions.StateDone && ev.Reason == "killed")
	to = terminalState(ev.To, killed)
	return from, to
}

// makeBootDirPlantedCallback returns a callback wired into
// agentsessions.StartOptions.OnBootDirPlanted. Lib v0.9.x invokes it
// once per session start after the planted boot dir is materialized.
// Payload schema: {"path":"<absolute>"} — see events.KindSessionBootDirPlanted.
// Returns a no-op when bus is nil (test composition that skips event wiring).
func makeBootDirPlantedCallback(bus events.Publisher, sessionID, logicalAgentID string) func(string) {
	return func(path string) {
		publishSessionEvent(bus, sessionID, logicalAgentID, events.KindSessionBootDirPlanted, struct {
			Path string `json:"path"`
		}{Path: path})
	}
}

// makeProviderSessionLostCallback publishes provider.session_lost when a
// resume turn ran in a new provider session instead of the requested one.
// The turn itself ran; the event is how callers learn its history is gone.
func makeProviderSessionLostCallback(bus events.Publisher, sessionID, logicalAgentID string) func(requested, actual, reason string) {
	return func(requested, actual, reason string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		publishSessionEventContext(ctx, bus, sessionID, logicalAgentID, events.KindProviderSessionLost, struct {
			Requested string `json:"requested"`
			Actual    string `json:"actual"`
			Reason    string `json:"reason"`
		}{requested, actual, reason})
	}
}

// makeProviderTypedEventCallback publishes provider.permission_denied for
// each tool action the provider refused headlessly, so the no-op is visible.
// Other typed events are ignored here; the attach stream already carries
// them.
func makeProviderTypedEventCallback(bus events.Publisher, sessionID, logicalAgentID string) gop.EventsCallback {
	return func(ev gopevents.Event) {
		d, ok := ev.(gopevents.PermissionDenied)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		publishSessionEventContext(ctx, bus, sessionID, logicalAgentID, events.KindProviderPermissionDenied, struct {
			Action      string `json:"action"`
			DisplayName string `json:"display_name"`
		}{d.Action, d.DisplayName})
	}
}

func publishSessionEvent(bus events.Publisher, sessionID, logicalAgentID, kind string, payload any) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	publishSessionEventContext(ctx, bus, sessionID, logicalAgentID, kind, payload)
}

func publishSessionEventContext(ctx context.Context, bus events.Publisher, sessionID, logicalAgentID, kind string, payload any) {
	if bus == nil {
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_ = bus.Publish(ctx, events.Event{
		Scope:          events.ScopeSession,
		SessionID:      sessionID,
		LogicalAgentID: logicalAgentID,
		Kind:           kind,
		PayloadJSON:    string(data),
	})
}

// Static interface checks. The assertions let the linter see the types
// as "used" and surface contract drift at build time if the lib's sink
// shapes change.
var (
	_ agentsessions.StateSink      = stateSinkAdapter{}
	_ agentsessions.AttachmentSink = attachmentSinkAdapter{}
	_ agentsessions.EventSink      = (*eventSinkAdapter)(nil)
)
