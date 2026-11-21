package app

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"os"
	"time"

	"github.com/hollis-labs/tether/internal/agencyregistry"
	"github.com/hollis-labs/tether/internal/runtoken"
	"github.com/hollis-labs/tether/internal/store"
	"go.opentelemetry.io/otel/trace"
)

// queueLaunchCorrelation does no remote IO, credential IO, or waiting. At most
// 32 launches may be in flight; overflow and exhausted retries are best effort.
func (s *Service) queueLaunchCorrelation(ctx context.Context, row *store.SessionRow, env map[string]string, providerSession, manifestDigest string) {
	if row.TaskID.String == "" && row.WorkstreamID.String == "" {
		return
	}
	cat := s.cat()
	if cat == nil || cat.Global.AgencyRegistry.URL == "" {
		return
	}
	cfg := cat.Global.AgencyRegistry
	facts := agencyregistry.Facts{URN: runURN(row), Session: row.ID, RunID: env[runtoken.IDEnv], TaskID: row.TaskID.String, ProviderSessionID: providerSession, ManifestDigest: manifestDigest}
	// Historical labels are not Registry identities. Preserve them locally, but
	// never describe them as Registry workstreams in new remote records.
	if store.IsRegistryWorkstreamID(row.WorkstreamID.String) {
		facts.WorkstreamID = row.WorkstreamID.String
	}
	token := env[runtoken.TokenEnv]
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		facts.Traceparent = "00-" + sc.TraceID().String() + "-" + sc.SpanID().String() + "-" + sc.TraceFlags().String()
	}
	if facts.RunID == "" {
		return
	}
	providerID := row.ProviderID
	s.correlationMu.Lock()
	if s.correlationClosed {
		s.correlationMu.Unlock()
		return
	}
	if s.correlationSlots == nil {
		s.correlationSlots = make(chan struct{}, 32)
		s.correlationContext, s.correlationCancel = context.WithCancel(context.Background())
	}
	select {
	case s.correlationSlots <- struct{}{}:
	default:
		s.correlationMu.Unlock()
		log.Print("Registry correlation dropped: capacity exhausted")
		return
	}
	workCtx, slots := s.correlationContext, s.correlationSlots
	s.correlationWG.Add(1)
	s.correlationMu.Unlock()
	go func() {
		defer s.correlationWG.Done()
		defer func() { <-slots }()
		if token != "" {
			key, err := s.runTokens.Key(s.Store.RunSigningKeyPath())
			if err == nil {
				if c, err := runtoken.Verify(key.Public().(ed25519.PublicKey), token, time.Now()); err == nil && c.Session == facts.Session && c.RunID == facts.RunID {
					facts.TokenJTI = c.JTI
				}
			}
		}
		if facts.ProviderSessionID == "" {
			if mapping, err := s.Store.GetSessionProviderMapping(facts.Session, "tether", providerID); err == nil {
				facts.ProviderSessionID = mapping.NativeSessionID.String
			}
		}
		key := "tether-launch:" + facts.Session + ":" + facts.RunID
		for attempt := 0; attempt < 3; attempt++ {
			attemptCtx, cancel := context.WithTimeout(workCtx, 2*time.Second)
			retry, err := agencyregistry.Register(attemptCtx, cfg, key, facts)
			cancel()
			if err == nil {
				return
			}
			if workCtx.Err() != nil {
				return
			}
			if !retry || attempt == 2 {
				log.Print("Registry correlation registration failed (best effort)")
				return
			}
			timer := time.NewTimer(time.Duration(attempt+1) * 250 * time.Millisecond)
			select {
			case <-workCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

func (s *Service) stopLaunchCorrelations() {
	s.correlationMu.Lock()
	s.correlationClosed = true
	if s.correlationCancel != nil {
		s.correlationCancel()
	}
	s.correlationMu.Unlock()
	s.correlationWG.Wait()
}

// Hash only the committed manifest bytes, never boot files or credentials.
// A missing manifest is an unknown fact, not a failed launch.
func launchManifestDigest(bootdir string) string {
	root, err := os.OpenRoot(bootdir)
	if err != nil {
		return ""
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat(".materialize/manifest.json")
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	f, err := root.Open(".materialize/manifest.json")
	if err != nil {
		return ""
	}
	defer f.Close()
	const limit = 2 << 20
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || len(data) > limit {
		return ""
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
