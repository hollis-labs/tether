package app

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/store"
)

// Daemon-only: the opened database chooses the identity file, before the bus,
// manager, reconciliation or any other event producer can be constructed.
func seedDaemonEventIdentity(ctx context.Context, db *store.Store, authority string) (store.EventIdentityProgress, error) {
	var path string
	if err := db.DB().QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		return store.EventIdentityProgress{}, err
	}
	if path == "" {
		return store.EventIdentityProgress{}, fmt.Errorf("event identity requires a persistent database")
	}
	id, err := environment.EnsureID(filepath.Dir(path))
	if err != nil {
		return store.EventIdentityProgress{}, err
	}

	p, err := db.SeedEventIdentity(ctx, id)
	if err != nil {
		return p, err
	}
	if err = environment.BindAuthority(filepath.Dir(path), id, authority); err != nil {
		return p, err
	}
	return p, nil
}

type eventIDBackfillStore interface {
	BackfillEventIDs(context.Context, int) (store.EventIdentityProgress, error)
	RecordEventIDBackfillFailure(context.Context) error
}

// Stop on the first failed chunk. Error text never enters progress or events;
// a later daemon restart resumes from the last committed cursor.
func runEventIDBackfill(ctx context.Context, db eventIDBackfillStore) {
	for {
		if ctx.Err() != nil {
			return
		}
		progress, err := db.BackfillEventIDs(ctx, 1000)
		if err != nil {
			if ctx.Err() == nil {
				countCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				if db.RecordEventIDBackfillFailure(countCtx) != nil {
					log.Printf("event ID backfill failure counter unavailable")
				}
				cancel()
				log.Printf("event ID backfill stopped; inspect readiness counts before restarting")
			}
			return
		}
		if progress.Remaining == 0 {
			return
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Service) StartEventIDBackfill(ctx context.Context) error {
	progress, err := s.Store.EventIdentityProgress(ctx)
	if err != nil {
		return err
	}
	s.eventIDsMu.Lock()
	defer s.eventIDsMu.Unlock()
	if s.eventIDsClosed {
		return fmt.Errorf("event ID backfill is closed")
	}
	if s.eventIDsDone != nil || progress.Remaining == 0 {
		return nil
	}
	ctx, s.eventIDsCancel = context.WithCancel(ctx)
	done := make(chan struct{})
	s.eventIDsDone = done
	go func() { defer close(done); runEventIDBackfill(ctx, s.Store) }()
	return nil
}

func (s *Service) stopEventIDBackfill() {
	s.eventIDsMu.Lock()
	s.eventIDsClosed = true
	cancel, done := s.eventIDsCancel, s.eventIDsDone
	s.eventIDsMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}
