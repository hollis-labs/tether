package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// growWAL writes enough event rows to push the -wal file well past a few MB.
func growWAL(t *testing.T, s *Store, rows int) {
	t.Helper()
	payload := `{"pad":"` + strings.Repeat("x", 4000) + `"}`
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < rows; i++ {
		if _, err := tx.Exec(`INSERT INTO events(scope,session_id,at,kind,payload_json) VALUES ('daemon','',?, 'test.pad', ?)`, time.Now().UTC().Format(time.RFC3339Nano), payload); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func walSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path + "-wal")
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// The writer connection carries journal_size_limit (CW-20261113-0009).
func TestWriterConnectionLimitsJournalSize(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var limit int64
	if err := s.db.QueryRow(`PRAGMA journal_size_limit`).Scan(&limit); err != nil {
		t.Fatal(err)
	}
	if limit != walSizeLimit {
		t.Fatalf("journal_size_limit = %d, want %d", limit, walSizeLimit)
	}
}

// TruncateWAL empties a grown -wal file, reports busy instead of waiting while
// a reader holds a snapshot, and leaves the writer's busy timeout as it was.
func TestTruncateWALEmptiesTheLogAndYieldsToAReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	growWAL(t, s, 2000)
	if size := walSize(t, path); size < 4<<20 {
		t.Fatalf("wal did not grow: %d bytes", size)
	}

	// A reader with an open snapshot on the history pool holds the log.
	rtx, err := s.reader.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := rtx.QueryRow(`SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	growWAL(t, s, 10)
	start := time.Now()
	res, err := s.TruncateWAL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Busy {
		t.Fatalf("truncate with a reader holding a snapshot = %+v, want busy", res)
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Fatalf("truncate waited %s for the reader", waited)
	}
	_ = rtx.Rollback()

	res, err = s.TruncateWAL(ctx)
	if err != nil || res.Busy {
		t.Fatalf("truncate = %+v, %v", res, err)
	}
	if size := walSize(t, path); size != 0 {
		t.Fatalf("wal after truncate = %d bytes, want 0", size)
	}
	var busy int
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busy); err != nil {
		t.Fatal(err)
	}
	if busy != 5000 {
		t.Fatalf("busy_timeout after truncate = %d, want 5000", busy)
	}
}
