package app

import (
	"context"
	"crypto/ed25519"
	"log"
	"time"

	"github.com/hollis-labs/tether/internal/correlationregistry"
	"github.com/hollis-labs/tether/internal/runtoken"
	"github.com/hollis-labs/tether/internal/store"
)

// registerLaunchCorrelation is deliberately fire-and-forget: neither network,
// credentials nor provider mapping reads can delay or fail session start. No
// durable retry promise is made; at most eight observations are in flight.
func (s *Service) registerLaunchCorrelation(row store.SessionRow, runEnv map[string]string) {
	cat := s.cat()
	if cat == nil || cat.Global.Daemon.CorrelationRegistry.URL == "" {
		return
	}
	cfg := cat.Global.Daemon.CorrelationRegistry
	s.correlationMu.Lock()
	if s.correlationSlots == nil {
		s.correlationSlots = make(chan struct{}, 8)
	}
	slots := s.correlationSlots
	s.correlationMu.Unlock()
	select {
	case slots <- struct{}{}:
	default:
		log.Print("correlation Registry observation dropped: capacity")
		return
	}
	// Copy only launcher-owned values before the goroutine. Never read the
	// launch plan's environment, nor serialize the token into a Registry record.
	execution, token := runEnv[runtoken.IDEnv], runEnv[runtoken.TokenEnv]
	go func() {
		defer func() { <-slots }()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		observation := correlationregistry.Observation{URN: runURN(&row), Session: row.ID, RunID: execution, DispatchRunID: row.RunID.String, TaskID: row.TaskID.String, WorkstreamID: row.WorkstreamID.String}
		if token != "" {
			key, err := s.runTokens.Key(s.Store.RunSigningKeyPath())
			if err == nil {
				claims, err := runtoken.Verify(key.Public().(ed25519.PublicKey), token, time.Now())
				if err == nil && claims.RunID == execution && claims.Session == row.ID {
					observation.JTI = claims.JTI
				}
			}
		}
		if mapping, err := s.Store.GetSessionProviderMapping(row.ID, "tether", row.ProviderID); err == nil {
			observation.ProviderSessionID = mapping.NativeSessionID.String
		}
		if err := correlationregistry.Register(ctx, cfg, []string{s.CatalogRoot}, observation); err != nil {
			log.Printf("correlation Registry observation failed: %v", err)
		}
	}()
}
