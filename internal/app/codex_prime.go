package app

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
)

// codexPrimeText is the throwaway first turn a Codex app-server team member
// takes at launch. A thread that has never run a turn did not wake for team
// deliveries on a live host, while one that had run a single turn did; giving
// every team member that turn up front makes the first delivery behave like
// any later one. It is kept short and asks for no work.
const codexPrimeText = "Ready. Reply with the single word ready, then wait for team messages."

// codexPrimeRetry bounds how long priming waits for the session to be able to
// take the turn (its host may still be settling after launch).
var (
	codexPrimeAttempts = 5
	codexPrimeBackoff  = 2 * time.Second
)

// primeAppServerSession gives a freshly launched app-server session its first
// turn. It never competes with real work: a session that is already
// processing, or that is gone, is left alone.
func (s *Service) primeAppServerSession(ctx context.Context, id string) error {
	var err error
	for attempt := 0; attempt < codexPrimeAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(codexPrimeBackoff):
			}
		}
		h, ok := s.RuntimeHealth(id)
		if !ok {
			return agentsessions.ErrSessionNotRunning
		}
		if h.Health.State == agentsessions.LiveStateProcessing {
			return nil // a real turn got there first
		}
		if err = s.SendTurn(ctx, id, codexPrimeText); err == nil {
			return nil
		}
		if errors.Is(err, agentsessions.ErrSessionNotRunning) {
			return err
		}
	}
	return err
}

// kickOffAppServerTeamMember primes sessionID once, if it is a fresh Codex
// app-server team member. It is called from the launch path and again when a
// team delivery is queued, so a session whose launch hook did not run (or did
// not take) is still primed before its first delivery. Idempotent per session.
func (s *Service) kickOffAppServerTeamMember(sessionID string) {
	if s.Store == nil || s.Manager == nil {
		return
	}
	plan, err := s.Store.GetLaunchPlan(sessionID)
	if err != nil || plan == nil || !plan.TeamMember || plan.ResumeSourceSessionID != "" || plan.ProviderBrand != "codex" {
		return
	}
	info, ok := s.Manager.Get(sessionID)
	if !ok || !info.Caps.JsonRpcStdio {
		return
	}
	if _, done := s.primedSessions.LoadOrStore(sessionID, struct{}{}); done {
		return
	}
	log.Printf("app: priming codex team session %s", sessionID)
	go func() {
		defer func() {
			// Forget on session end so the map cannot grow without bound.
			_, _ = s.Manager.WaitSession(context.Background(), sessionID)
			s.primedSessions.Delete(sessionID)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.primeAppServerSession(ctx, sessionID); err != nil {
			log.Printf("app: prime codex team session %s: %v", sessionID, err)
		}
	}()
}
