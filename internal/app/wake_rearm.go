package app

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/hollis-labs/libs/mesh/core/messaging/delivery"

	"github.com/hollis-labs/tether/internal/events"
)

// Busy-seat wakes are re-armed on turn end (CW-20261117-0002). A wake that
// finds its session mid-turn is Nacked with a short capped backoff and noted
// here; when the session's turn ends the noted deliveries become claimable at
// once, so the next sweep tick delivers the wake instead of the wake giving
// up after a fixed number of attempts.

// wakeRearmMaxPerSession bounds the deliveries remembered per session.
const wakeRearmMaxPerSession = 256

// wakeRearmSet maps a session to the deliveries whose wake it refused as busy.
// It is in memory: a daemon restart forgets it, and the capped busy backoff
// then delivers the wake within wakeBusyRetryCap. The zero value is ready.
type wakeRearmSet struct {
	mu      sync.Mutex
	pending map[string]map[delivery.DeliveryID]struct{}
}

func (r *wakeRearmSet) add(sessionID string, id delivery.DeliveryID) {
	if sessionID == "" || id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil {
		r.pending = map[string]map[delivery.DeliveryID]struct{}{}
	}
	set := r.pending[sessionID]
	if set == nil {
		set = map[delivery.DeliveryID]struct{}{}
		r.pending[sessionID] = set
	}
	if len(set) < wakeRearmMaxPerSession {
		set[id] = struct{}{}
	}
}

// take removes and returns the session's noted deliveries.
func (r *wakeRearmSet) take(sessionID string) []delivery.DeliveryID {
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.pending[sessionID]
	delete(r.pending, sessionID)
	out := make([]delivery.DeliveryID, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// rearmBusyWakes makes the session's busy-refused wakes claimable now. Called
// on the session's turn end.
func (s *Service) rearmBusyWakes(sessionID string) {
	ids := s.wakeRearm.take(sessionID)
	if len(ids) == 0 || s.Store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	n, err := s.Store.RearmDeliveries(ctx, ids)
	if err != nil {
		log.Printf("app: rearm busy wakes for session %s: %v", sessionID, err)
		return
	}
	if n > 0 {
		log.Printf("app: turn end re-armed %d busy wake(s) for session %s", n, sessionID)
	}
}

// KindMailTriageBackfill is published once per daemon when the reply_to_own
// backfill moved rows: {examined, moved, seat_count}.
const KindMailTriageBackfill = "mail.triage_backfill"

// runReplyToOwnBackfill re-classifies pending fyi replies to a seat's own mail
// as action (CW-20261117-0002); the sweep that follows wakes each affected seat
// once with a coalesced digest. Idempotent; logged and published when it moved
// anything.
func (s *Service) runReplyToOwnBackfill(ctx context.Context) {
	if s.mailTriager == nil {
		return
	}
	examined, moved, seats, err := s.mailTriager.backfillReplyToOwn(ctx)
	if err != nil {
		log.Printf("app: reply_to_own backfill: %v (examined %d, moved %d)", err, examined, moved)
		return
	}
	if moved == 0 {
		return
	}
	log.Printf("app: reply_to_own backfill: examined %d pending fyi replies, moved %d to action across %d seat(s)", examined, moved, len(seats))
	if s.Bus == nil {
		return
	}
	raw, _ := json.Marshal(map[string]any{"examined": examined, "moved": moved, "seat_count": len(seats)})
	e := events.Event{Scope: events.ScopeDaemon, Kind: KindMailTriageBackfill, At: time.Now().UTC(), PayloadJSON: string(raw)}
	e.Envelope.Class.Category = "message"
	if err := s.Bus.Publish(context.WithoutCancel(ctx), e); err != nil {
		log.Printf("app: publish %s: %v", KindMailTriageBackfill, err)
	}
}
