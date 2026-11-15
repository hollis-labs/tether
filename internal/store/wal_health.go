package store

import (
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"sync"
	"time"
)

// WALStatus is the state database's write-ahead log as /health reports it
// (CW-20261114-0166). A WAL that keeps growing while its checkpoint sequence
// stands still means a reader is holding back checkpoints: the 3.4 GB WAL of
// 2026-11-14.
type WALStatus struct {
	// WALBytes is the size of the -wal file. SQLite reuses the file from the
	// start after a checkpoint, so it shrinks only when truncated.
	WALBytes int64 `json:"wal_bytes"`
	// CheckpointSeq is the WAL header's checkpoint sequence number, which
	// SQLite advances each time it restarts the log after a full checkpoint.
	CheckpointSeq uint32 `json:"checkpoint_seq"`
	// LastCheckpointAgeSec is how long CheckpointSeq has stood still, as
	// sampled by this daemon. Until a restart has been seen
	// (LastCheckpointObserved false) it counts from the first sample, so it
	// is a lower bound.
	LastCheckpointAgeSec   int64 `json:"last_checkpoint_age_sec"`
	LastCheckpointObserved bool  `json:"last_checkpoint_observed"`
}

// walMonitor remembers the last checkpoint sequence it saw and when it changed.
type walMonitor struct {
	mu        sync.Mutex
	known     bool
	seq       uint32
	changedAt time.Time
	observed  bool
}

// walMagic is the first word of a WAL header (either byte order of checksums).
const (
	walMagicLE = 0x377f0682
	walMagicBE = 0x377f0683
)

// WALStatus samples the state database's WAL. Call it periodically (the
// daemon does, every walSampleInterval) so a log restart between two /health
// requests is still seen.
func (s *Store) WALStatus(now time.Time) (WALStatus, error) {
	if s.stateDBPath == "" {
		return WALStatus{}, errors.New("no state database path")
	}
	size, seq, err := readWALHeader(s.stateDBPath + "-wal")
	if err != nil {
		return WALStatus{}, err
	}
	m := &s.wal
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case size == 0:
		// No log, or one truncated after a full checkpoint: nothing is pending.
		m.known, m.seq, m.changedAt, m.observed = true, seq, now, true
	case !m.known:
		m.known, m.seq, m.changedAt = true, seq, now
	case seq != m.seq:
		m.seq, m.changedAt, m.observed = seq, now, true
	}
	return WALStatus{WALBytes: size, CheckpointSeq: m.seq, LastCheckpointAgeSec: int64(now.Sub(m.changedAt) / time.Second), LastCheckpointObserved: m.observed}, nil
}

// readWALHeader returns the WAL file's size and its checkpoint sequence
// number (0 when the file is missing, empty or has no valid header).
func readWALHeader(path string) (int64, uint32, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is the daemon's own state database plus "-wal"
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	var hdr [32]byte
	if _, err = io.ReadFull(f, hdr[:]); err != nil {
		return info.Size(), 0, nil //nolint:nilerr // a log shorter than its header has no checkpoint sequence yet
	}
	if magic := binary.BigEndian.Uint32(hdr[0:4]); magic != walMagicLE && magic != walMagicBE {
		return info.Size(), 0, nil
	}
	return info.Size(), binary.BigEndian.Uint32(hdr[12:16]), nil
}
