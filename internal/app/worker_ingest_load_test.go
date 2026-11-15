package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerhub"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// Load test of the worker output ingest path (CW-20261114-0120): many sessions
// times many frames through ingestWorkerFrame, reporting frames/s. CI runs it
// small; for numbers, scale it and point it at a real disk and database:
//
//	TETHER_INGEST_LOAD_FRAMES  frames per session (default 20)
//	TETHER_INGEST_LOAD_DIR     where the session workspaces go (default a temp dir)
//	TETHER_INGEST_LOAD_DB      an existing state database to ingest into (a copy!)
//
// Two shapes: serial (one goroutine, sessions round-robin: how the hub feeds one
// worker today) and parallel (one goroutine per session: several workers, or a
// fanned-out hub).
func TestWorkerIngestLoad(t *testing.T) {
	frames := envInt(t, "TETHER_INGEST_LOAD_FRAMES", 20)
	for _, sessions := range []int{1, 20} {
		for _, parallel := range []bool{false, true} {
			if sessions == 1 && parallel {
				continue
			}
			name := fmt.Sprintf("%d-sessions-%s", sessions, map[bool]string{false: "serial", true: "parallel"}[parallel])
			t.Run(name, func(t *testing.T) {
				svc, ids := ingestLoadRig(t, sessions)
				data := []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"a line of agent output about 120 bytes"}]}}` + "\n")
				start := time.Now()
				run := func(id string, from int) error {
					for seq := from; seq < from+frames; seq++ {
						if err := svc.ingestWorkerFrame("w-load", workerwire.Frame{T: "output", Session: id, Seq: int64(seq), Stream: "stdout", Data: data}); err != nil {
							return fmt.Errorf("%s seq %d: %w", id, seq, err)
						}
					}
					return nil
				}
				var err error
				if parallel {
					var wg sync.WaitGroup
					errs := make(chan error, len(ids))
					for _, id := range ids {
						wg.Go(func() { errs <- run(id, 1) })
					}
					wg.Wait()
					close(errs)
					for e := range errs {
						err = errorsJoin(err, e)
					}
				} else {
					for seq := 1; seq <= frames && err == nil; seq++ {
						for _, id := range ids {
							if err = svc.ingestWorkerFrame("w-load", workerwire.Frame{T: "output", Session: id, Seq: int64(seq), Stream: "stdout", Data: data}); err != nil {
								break
							}
						}
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				elapsed := time.Since(start)
				total := sessions * frames
				t.Logf("CW-20261114-0120 load: %s: %d frames in %s = %.0f frames/s (%.2f ms/frame)", name, total, elapsed.Round(time.Millisecond), float64(total)/elapsed.Seconds(), float64(elapsed.Microseconds())/1000/float64(total))
			})
		}
	}
}

func envInt(t *testing.T, key string, def int) int {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		t.Fatalf("%s=%q", key, raw)
	}
	return n
}

func errorsJoin(a, b error) error {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return fmt.Errorf("%w; %w", a, b)
}

// ingestLoadRig is a service whose worker "w-load" owns n running sessions.
func ingestLoadRig(t *testing.T, n int) (*Service, []string) {
	t.Helper()
	dbPath := os.Getenv("TETHER_INGEST_LOAD_DB")
	if dbPath == "" {
		dbPath = filepath.Join(t.TempDir(), "state.db")
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	bus := events.NewBus(events.BusOptions{Persister: db})
	svc := &Service{Store: db, Bus: bus, Workers: workerhub.New(db.DB())}
	dir := os.Getenv("TETHER_INGEST_LOAD_DIR")
	if dir == "" {
		dir = t.TempDir()
	} else {
		dir, err = os.MkdirTemp(dir, "ingest-load-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}
	stamp := strconv.FormatInt(time.Now().UnixNano(), 36)
	ids := make([]string, 0, n)
	for i := range n {
		id := fmt.Sprintf("load-%s-%d", stamp, i)
		row := store.SessionRow{ID: id, LaunchID: "l1", ProjectID: "p", ProviderID: "stub", Workspace: filepath.Join(dir, id), State: string(session.StateRunning), WorkerID: "w-load"}
		if err = db.CreateSession(row, &launch.Plan{LaunchID: "l1"}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return svc, ids
}
