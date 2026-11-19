package app

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// syncedSizes records, per log path, the size afterSessionLogSync reported.
type syncedSizes struct {
	mu    sync.Mutex
	sizes map[string]int64
	syncs int
}

func watchSyncs(t *testing.T) *syncedSizes {
	t.Helper()
	w := &syncedSizes{sizes: map[string]int64{}}
	afterSessionLogSync = func(path string, size int64) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.sizes[path] = size
		w.syncs++
	}
	t.Cleanup(func() { afterSessionLogSync = nil })
	return w
}

func (w *syncedSizes) get(path string) (int64, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sizes[path], w.syncs
}

func groupCommit(t *testing.T, frames int, delay time.Duration) {
	t.Helper()
	oldFrames, oldDelay := sessionLogSyncFrames, sessionLogSyncDelay
	sessionLogSyncFrames, sessionLogSyncDelay = frames, delay
	t.Cleanup(func() { sessionLogSyncFrames, sessionLogSyncDelay = oldFrames, oldDelay })
}

func frameLine(session string, seq int) []byte {
	return []byte(fmt.Sprintf("%s frame %04d\n", session, seq))
}

func ingestFrames(t *testing.T, svc *Service, id string, from, to int) {
	t.Helper()
	for seq := from; seq <= to; seq++ {
		if err := svc.ingestWorkerFrame("w-load", workerwire.Frame{T: "output", Session: id, Seq: int64(seq), Stream: "stderr", Data: frameLine(id, seq)}); err != nil {
			t.Fatalf("%s seq %d: %v", id, seq, err)
		}
	}
}

func sessionOutputCount(t *testing.T, svc *Service, id string) int {
	t.Helper()
	var n int
	if err := svc.Store.DB().QueryRow(`SELECT count(*) FROM events WHERE session_id=? AND kind='session.output'`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// CW-20261114-0120 durability contract: on a crash, session.log loses at most
// its un-synced tail (fewer than sessionLogSyncFrames frames); the cursor and
// the session.output events, which SQLite committed, are unaffected; and ingest
// carries on after the restart. The crash is simulated at the only level a test
// can: the open logs are dropped unsynced and each file is cut back to the size
// last reported synced, which is exactly what an OS crash may keep.
func TestSessionLogCrashLosesAtMostTheUnsyncedTail(t *testing.T) {
	groupCommit(t, 8, time.Hour)
	synced := watchSyncs(t)
	svc, ids := ingestLoadRig(t, 1)
	id := ids[0]
	row, err := svc.Store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(row.Workspace, "logs", "session.log")
	ingestFrames(t, svc, id, 1, 20)

	// Crash.
	svc.sessionLogs.abandon()
	size, _ := synced.get(path)
	if err = os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want []byte
	for seq := 1; seq <= 16; seq++ {
		want = append(want, frameLine(id, seq)...)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("after the crash session.log is %d bytes:\n%s\nwant frames 1-16 (20 written, synced every 8)", len(got), got)
	}
	lost := 20 - strings.Count(string(got), "\n")
	if lost >= sessionLogSyncFrames {
		t.Fatalf("lost %d frames, more than one batch", lost)
	}
	if next := svc.Workers.NextSeq(id); next != 21 {
		t.Fatalf("cursor after the crash: next seq %d, want 21", next)
	}
	if n := sessionOutputCount(t, svc, id); n != 20 {
		t.Fatalf("%d session.output events after the crash, want 20", n)
	}

	// Restart: ingest continues from the cursor, appending to the surviving log.
	ingestFrames(t, svc, id, 21, 24)
	svc.sessionLogs.closeAll()
	got, _ = os.ReadFile(path)
	for seq := 21; seq <= 24; seq++ {
		want = append(want, frameLine(id, seq)...)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("after the restart:\n%s", got)
	}
	if next := svc.Workers.NextSeq(id); next != 25 || sessionOutputCount(t, svc, id) != 24 {
		t.Fatalf("after the restart: next seq %d, events %d", next, sessionOutputCount(t, svc, id))
	}
}

// Fewer frames than a batch are synced within the delay.
func TestSessionLogSyncsWithinTheDelay(t *testing.T) {
	groupCommit(t, 1000, 50*time.Millisecond)
	synced := watchSyncs(t)
	svc, ids := ingestLoadRig(t, 1)
	ingestFrames(t, svc, ids[0], 1, 3)
	row, _ := svc.Store.GetSession(ids[0])
	path := filepath.Join(row.Workspace, "logs", "session.log")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if size, _ := synced.get(path); size == info.Size() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("three frames were not synced within the delay")
		}
		time.Sleep(10 * time.Millisecond)
	}
	svc.sessionLogs.closeAll()
}

// Sessions ingest in parallel, each in its own order; a gap is still refused.
func TestParallelSessionsKeepTheirOrder(t *testing.T) {
	groupCommit(t, 8, 20*time.Millisecond)
	svc, ids := ingestLoadRig(t, 10)
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() { ingestFrames(t, svc, id, 1, 50) })
	}
	wg.Wait()
	svc.sessionLogs.closeAll()
	for _, id := range ids {
		row, _ := svc.Store.GetSession(id)
		got, err := os.ReadFile(filepath.Join(row.Workspace, "logs", "session.log"))
		if err != nil {
			t.Fatal(err)
		}
		var want []byte
		for seq := 1; seq <= 50; seq++ {
			want = append(want, frameLine(id, seq)...)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s log out of order or mixed:\n%s", id, got)
		}
		if svc.Workers.NextSeq(id) != 51 || sessionOutputCount(t, svc, id) != 50 {
			t.Fatalf("%s: next seq %d, events %d", id, svc.Workers.NextSeq(id), sessionOutputCount(t, svc, id))
		}
	}
	err := svc.ingestWorkerFrame("w-load", workerwire.Frame{T: "output", Session: ids[0], Seq: 53, Stream: "stderr", Data: []byte("x\n")})
	if err == nil || !strings.Contains(err.Error(), "sequence gap") {
		t.Fatalf("a gap was accepted: %v", err)
	}
}

// A codex permission-source session's stdout is synced before its spans
// commit, so the parser never stores a span past what survives a crash.
func TestPermissionSourceFramesAreSyncedBeforeTheirSpans(t *testing.T) {
	groupCommit(t, 1000, time.Hour)
	synced := watchSyncs(t)
	svc, _ := ingestLoadRig(t, 0)
	dir := t.TempDir()
	row := store.SessionRow{ID: "codex-1", LaunchID: "l1", ProjectID: "p", ProviderID: "codex", Workspace: dir, State: string(session.StateRunning), WorkerID: "w-load"}
	if err := svc.Store.CreateSession(row, &launch.Plan{LaunchID: "l1", ProviderBrand: "codex", RuntimeKind: "jsonrpc-stdio"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "logs", "session.log")
	for seq := 1; seq <= 3; seq++ {
		if err := svc.ingestWorkerFrame("w-load", workerwire.Frame{T: "output", Session: "codex-1", Seq: int64(seq), Stream: "stdout", Data: []byte(`{"partial":`)}); err != nil {
			t.Fatal(err)
		}
		info, _ := os.Stat(path)
		if size, _ := synced.get(path); size != info.Size() {
			t.Fatalf("frame %d: synced %d of %d bytes before its span", seq, size, info.Size())
		}
	}
	svc.sessionLogs.closeAll()
}

// The same session fed twice at once (overlapping replays) still ingests each
// frame exactly once, in order: the session's lock serializes the sequence
// check, the log append and the cursor commit.
func TestOneSessionFedConcurrentlyIngestsEachFrameOnce(t *testing.T) {
	groupCommit(t, 8, 20*time.Millisecond)
	svc, ids := ingestLoadRig(t, 1)
	id := ids[0]
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { ingestFrames(t, svc, id, 1, 200) })
	}
	wg.Wait()
	svc.sessionLogs.closeAll()
	row, _ := svc.Store.GetSession(id)
	got, _ := os.ReadFile(filepath.Join(row.Workspace, "logs", "session.log"))
	var want []byte
	for seq := 1; seq <= 200; seq++ {
		want = append(want, frameLine(id, seq)...)
	}
	if !bytes.Equal(got, want) || sessionOutputCount(t, svc, id) != 200 {
		t.Fatalf("log %d bytes (want %d), events %d", len(got), len(want), sessionOutputCount(t, svc, id))
	}
}
