package app

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

// Seat wait kinds (CW-20261114-0019): a seat is blocked on a human or another
// agent, and the wait ended. Payload {waiting_for, request_id?, resolution?}.
const (
	KindSeatWaiting   = "seat.waiting"
	KindSeatUnblocked = "seat.unblocked"
)

// seatWaitTracker derives seat.waiting / seat.unblocked from the
// session.request_status events the runtime request path already publishes. A
// session waits while it has at least one open approval or question: the first
// one opening emits seat.waiting, the last one closing (resolved, or closed by
// the turn ending) emits seat.unblocked. The open set is in memory, so a
// request that was open across a daemon restart closes without an unblocked;
// the request_status rows remain the record.
type seatWaitTracker struct {
	mu   sync.Mutex
	pub  events.Publisher
	open map[string]map[string]string // session -> request id -> request kind
}

func newSeatWaitTracker() *seatWaitTracker {
	return &seatWaitTracker{open: map[string]map[string]string{}}
}

func (t *seatWaitTracker) SetPublisher(p events.Publisher) {
	t.mu.Lock()
	t.pub = p
	t.mu.Unlock()
}

type seatWait struct {
	WaitingFor string `json:"waiting_for,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
	Resolution string `json:"resolution,omitempty"`
}

// Observe implements events.Observer.
func (t *seatWaitTracker) Observe(ctx context.Context, e events.Event) {
	if e.SessionID == "" {
		return
	}
	kind, payload := t.next(e)
	if kind == "" {
		return
	}
	t.mu.Lock()
	pub := t.pub
	t.mu.Unlock()
	if pub == nil {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	out := events.Event{Scope: events.ScopeSession, SessionID: e.SessionID, LogicalAgentID: e.LogicalAgentID, Kind: kind, At: time.Now().UTC(), PayloadJSON: string(raw)}
	out.Envelope.Dims = e.Envelope.Dims
	out.Envelope.Class.Category = "state"
	if err := pub.Publish(context.WithoutCancel(ctx), out); err != nil {
		log.Printf("app: publish %s for session %s: %v", kind, e.SessionID, err)
	}
}

// next updates the open set for e and returns the event to publish, if any.
func (t *seatWaitTracker) next(e events.Event) (string, seatWait) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e.Kind == events.KindSessionStateChanged {
		var p struct {
			To string `json:"to"`
		}
		_ = json.Unmarshal([]byte(e.PayloadJSON), &p)
		switch p.To {
		case "completed", "failed", "killed", "stopped":
			return t.closeAll(e.SessionID, "session_ended")
		}
		return "", seatWait{}
	}
	if e.Kind != store.EnvironmentRequestKind {
		return "", seatWait{}
	}
	var req store.EnvironmentRequest
	if json.Unmarshal([]byte(e.PayloadJSON), &req) != nil {
		return "", seatWait{}
	}
	if req.Terminal {
		return t.closeAll(e.SessionID, "turn_ended")
	}
	if req.RequestID == "" {
		return "", seatWait{}
	}
	open := t.open[e.SessionID]
	if req.Open {
		if open == nil {
			open = map[string]string{}
			t.open[e.SessionID] = open
		}
		_, known := open[req.RequestID]
		open[req.RequestID] = req.Kind
		if len(open) == 1 && !known {
			return KindSeatWaiting, seatWait{WaitingFor: req.Kind, RequestID: req.RequestID}
		}
		return "", seatWait{}
	}
	kind, known := open[req.RequestID]
	if !known {
		return "", seatWait{}
	}
	delete(open, req.RequestID)
	if len(open) > 0 {
		return "", seatWait{}
	}
	delete(t.open, e.SessionID)
	return KindSeatUnblocked, seatWait{WaitingFor: kind, RequestID: req.RequestID, Resolution: "resolved"}
}

func (t *seatWaitTracker) closeAll(session, resolution string) (string, seatWait) {
	open := t.open[session]
	if len(open) == 0 {
		return "", seatWait{}
	}
	delete(t.open, session)
	// One request names itself; several name their kind only when they share it.
	w := seatWait{Resolution: resolution}
	first := true
	for id, kind := range open {
		if len(open) == 1 {
			w.RequestID = id
		}
		if first {
			w.WaitingFor, first = kind, false
		} else if w.WaitingFor != kind {
			w.WaitingFor = ""
		}
	}
	return KindSeatUnblocked, w
}
