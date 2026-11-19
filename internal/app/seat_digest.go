package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/messaging/wakeintent"
	"github.com/hollis-labs/tether/internal/store"
)

// Seat digest wakes (CW-20261117-0002). The wake sweep walks deliveries, so
// pending action mail whose delivery is not ready never wakes its seat: a
// delivery an adapter already acknowledged as delivered (a worker seat) while
// triage held the wake, or one buried behind a deep backlog. This pass wakes
// such a seat directly: its live, idle session gets one turn listing all of
// its pending mail, and the listed mail is settled (covered / surfaced). It
// keeps the per-seat action interval, and a seat with no live session is
// woken when one comes up.

var (
	// seatDigestInterval is how often the pass runs at most.
	seatDigestInterval = 30 * time.Second
	// seatDigestGrace is how long a pending action is left to the delivery
	// sweep before the seat pass may take it.
	seatDigestGrace = 2 * time.Minute
)

type seatDigestState struct {
	mu   sync.Mutex
	last time.Time
}

// due reports whether the pass may run now, and records it.
func (d *seatDigestState) due(now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.last.IsZero() && now.Sub(d.last) < seatDigestInterval {
		return false
	}
	d.last = now
	return true
}

func (s *Service) runSeatDigestWakes(ctx context.Context) {
	if s.mailTriager == nil || !s.seatDigest.due(time.Now()) {
		return
	}
	n, err := runSeatDigest(ctx, s.Store, s.mailTriager, s.runtimeSeam(), s.ResolveActorSession)
	if err != nil {
		log.Printf("app: seat digest wakes: %v", err)
	} else if n > 0 {
		log.Printf("app: seat digest woke %d seat(s) with pending mail no wake had named", n)
	}
}

// runSeatDigest wakes each triaged seat that has stranded action mail and a
// live idle session; it returns how many seats it woke.
func runSeatDigest(ctx context.Context, st *store.Store, tri *mailTriage, rt wakeRuntime, resolve func(context.Context, string) (string, error)) (int, error) {
	if st == nil || tri == nil || tri.policy == nil {
		return 0, nil
	}
	now := tri.clock()
	seats, err := st.TriageStrandedSeats(ctx, now.Add(-seatDigestGrace), now)
	if err != nil {
		return 0, err
	}
	woke := 0
	for _, seat := range seats {
		addr, err := messaging.ParseURN(seat)
		if err != nil {
			continue
		}
		pol := tri.policy(addr)
		if !pol.enabled || pol.agentID == "" {
			continue
		}
		if last, err := st.SeatLastActionWake(ctx, pol.seat); err != nil || (!last.IsZero() && pol.interval > 0 && now.Before(last.Add(pol.interval))) {
			continue
		}
		sessionID, err := resolve(ctx, pol.agentID)
		if err != nil || sessionID == "" || strings.HasPrefix(sessionID, "outpost:") {
			continue
		}
		health, ok := rt.health(sessionID)
		if !ok || health.Health.State == agentsessions.LiveStateProcessing {
			continue // offline or busy: a later pass tries again
		}
		items, total, err := st.TriageSeatPending(ctx, pol.seat, 1)
		if err != nil || total == 0 {
			continue
		}
		list, commit := tri.digest(ctx, messaging.Envelope{To: addr})
		if list == "" {
			continue
		}
		text := fmt.Sprintf("**Mailbox wake (daemon-injected, digest)** — mail for you that no wake has named yet.\n\n%s\n\n%s", list, wakeintent.WakeGuidance)
		wctx := withTurnInput(ctx, turnInputNote{Source: store.InputMailWake, Class: store.ClassSystem, Basis: store.BasisSystem, RefID: items[0].MessageID})
		if err := rt.sendTurn(wctx, sessionID, text); err != nil {
			log.Printf("app: seat digest wake of %s (session %s): %v", pol.seat, sessionID, err)
			continue
		}
		commit()
		_ = st.SetSeatLastActionWake(context.WithoutCancel(ctx), pol.seat, now)
		woke++
	}
	return woke, nil
}
