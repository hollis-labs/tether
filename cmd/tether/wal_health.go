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

// walTruncateEvery spaces out TRUNCATE checkpoints while the WAL stays over
// store.WALTruncateThreshold, so a reader that keeps it busy is not retried on
// every sample.
const walTruncateEvery = time.Minute

// sampleWAL samples the WAL until ctx ends. While the -wal file is over
// store.WALTruncateThreshold it also asks for a TRUNCATE checkpoint, at most
// once per walTruncateEvery (CW-20261113-0009).
func sampleWAL(ctx context.Context, db *store.Store) {
	tick := time.NewTicker(walSampleInterval)
	defer tick.Stop()
	var lastTruncate time.Time
	for {
		now := time.Now()
		st, err := db.WALStatus(now)
		if err != nil {
			log.Printf("wal health: %v", err)
		} else if st.WALBytes > store.WALTruncateThreshold && now.Sub(lastTruncate) >= walTruncateEvery {
			lastTruncate = now
			res, err := db.TruncateWAL(ctx)
			switch {
			case err != nil:
				log.Printf("wal health: truncate checkpoint: %v", err)
			case res.Busy:
				log.Printf("wal health: truncate checkpoint busy (wal %d bytes, %d/%d frames checkpointed); retrying in %s", st.WALBytes, res.Checkpointed, res.Log, walTruncateEvery)
			default:
				after, _ := db.WALStatus(time.Now())
				log.Printf("wal health: truncate checkpoint done (wal %d -> %d bytes)", st.WALBytes, after.WALBytes)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
