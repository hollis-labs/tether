package main

import (
	"context"
	"log"
	"time"

	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/store"
)

// walSampleInterval is how often the daemon samples the state database's WAL
// header, so a log restart between two /health requests is still seen
// (CW-20261114-0166).
const walSampleInterval = 15 * time.Second

// walHealth is /health's WAL block; nil when the WAL cannot be read.
func walHealth(db *store.Store, now time.Time) *daemon.WALHealth {
	st, err := db.WALStatus(now)
	if err != nil {
		return nil
	}
	return &daemon.WALHealth{WALBytes: st.WALBytes, CheckpointSeq: st.CheckpointSeq, LastCheckpointAgeSec: st.LastCheckpointAgeSec, LastCheckpointObserved: st.LastCheckpointObserved}
}

// sampleWAL samples the WAL until ctx ends.
func sampleWAL(ctx context.Context, db *store.Store) {
	tick := time.NewTicker(walSampleInterval)
	defer tick.Stop()
	for {
		if _, err := db.WALStatus(time.Now()); err != nil {
			log.Printf("wal health: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
