package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

func TestDaemonEventIdentitySeedFrozenAndFileMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.DB().Exec(`INSERT INTO events(scope,at,kind) VALUES('daemon','2026-01-01T00:00:00Z','legacy')`); err != nil {
		t.Fatal(err)
	}
	p, err := seedDaemonEventIdentity(context.Background(), db, "")
	if err != nil || p.Remaining != 1 || p.Cutover != 1 {
		t.Fatal(p, err)
	}
	id, err := environment.EnsureID(filepath.Dir(path))
	if err != nil || p.EnvironmentID != id {
		t.Fatal(id, p, err)
	}
	if _, _, err := db.InsertEvent("daemon", "", "new", "{}"); err != nil {
		t.Fatal(err)
	}
	again, err := seedDaemonEventIdentity(context.Background(), db, "")
	if err != nil || again != p {
		t.Fatal("seed moved cutover", again, err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "environment-id"), []byte("c915eb12-e0bc-444d-9dd3-ce73a0880495\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := seedDaemonEventIdentity(context.Background(), db, ""); !errors.Is(err, store.ErrEventIdentityMismatch) {
		t.Fatal("file mismatch accepted", err)
	}
}

type controlledIDBackfill struct {
	entered chan int
	release chan struct{}
	failed  int
	fail    bool
}

func (f *controlledIDBackfill) BackfillEventIDs(ctx context.Context, n int) (store.EventIdentityProgress, error) {
	f.entered <- n
	select {
	case <-ctx.Done():
		return store.EventIdentityProgress{}, ctx.Err()
	case <-f.release:
	}
	if f.fail {
		return store.EventIdentityProgress{}, errors.New("private-error-content")
	}
	return store.EventIdentityProgress{}, nil
}
func (f *controlledIDBackfill) RecordEventIDBackfillFailure(context.Context) error {
	f.failed++
	return nil
}
func TestEventIDBackfillCancellationAndFailureCounts(t *testing.T) {
	for _, failure := range []bool{false, true} {
		f := &controlledIDBackfill{entered: make(chan int, 1), release: make(chan struct{}), fail: failure}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); runEventIDBackfill(ctx, f) }()
		if n := <-f.entered; n != 1000 {
			t.Fatal("chunk limit", n)
		}
		if failure {
			close(f.release)
		} else {
			cancel()
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("backfill failed to join")
		}
		cancel()
		want := 0
		if failure {
			want = 1
		}
		if f.failed != want {
			t.Fatal("wrong failure counter", f.failed)
		}
	}
}

func TestEventIDBackfillFailureRestartAndJoin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.DB().Exec(`INSERT INTO events(scope,at,kind) VALUES('daemon','2026-01-01T00:00:00Z','legacy')`); err != nil {
		t.Fatal(err)
	}
	if _, err := seedDaemonEventIdentity(context.Background(), db, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`CREATE TRIGGER fail_backfill BEFORE UPDATE OF event_id ON events BEGIN SELECT RAISE(ABORT,'private-error-content'); END`); err != nil {
		t.Fatal(err)
	}
	s := &Service{Store: db}
	if err := s.StartEventIDBackfill(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-s.eventIDsDone
	s.stopEventIDBackfill()
	p, err := db.EventIdentityProgress(context.Background())
	if err != nil || p.Failed != 1 || p.Backfilled != 0 || p.Remaining != 1 {
		t.Fatal(p, err)
	}
	if _, err := db.DB().Exec(`DROP TRIGGER fail_backfill`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted := &Service{Store: db}
	if err := restarted.StartEventIDBackfill(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-restarted.eventIDsDone
	restarted.stopEventIDBackfill()
	p, err = db.EventIdentityProgress(context.Background())
	if err != nil || p.Failed != 1 || p.Backfilled != 1 || p.Remaining != 0 {
		t.Fatal(p, err)
	}
}

func TestDaemonEventCutoverConstructorBeforeProducer(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`INSERT INTO events(scope,at,kind) VALUES('daemon','2026-01-01T00:00:00Z','legacy')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if err := os.WriteFile(filepath.Join(root, "global.yaml"), []byte("catalog:\n  defaults:\n    state_db: "+path+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	svc, err := NewDaemon(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc.Close() }()
	p, err := svc.Store.EventIdentityProgress(context.Background())
	if err != nil || p.Cutover != 1 || p.Remaining != 1 {
		t.Fatal(p, err)
	}
	if err := svc.Bus.Publish(context.Background(), events.Event{Scope: events.ScopeDaemon, Kind: "first.producer", PayloadJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	es, err := svc.Store.EventsSince(1)
	if err != nil || len(es) != 1 || es[0].Envelope.Dims.EnvironmentID != p.EnvironmentID {
		t.Fatal("producer preceded identity/process stamp", es, err)
	}
}
