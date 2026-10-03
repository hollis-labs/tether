package teamstore_test

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamstore"
)

func launch(key string) teams.LaunchRecord {
	return teams.LaunchRecord{Key: key, Digest: "digest", State: teams.Planning, Team: definition(), Limits: limits(), Deadline: time.Now().Add(time.Minute), Intents: []teams.MemberIntent{{Key: key + "-worker", MemberID: "worker", Slot: definition().Slots[0]}}}
}
func put(t *testing.T, s *teamstore.Store, r teams.LaunchRecord) {
	t.Helper()
	must(t, s.WithLease(context.Background(), r.Key, func(ctx context.Context) error { return s.PutLaunch(ctx, r) }))
}
func TestLaunchJournalAndTransitions(t *testing.T) {
	ctx := context.Background()
	_, s := open(t, t.TempDir()+"/state.db", teamstore.Options{})
	r := launch("a")
	if err := s.PutLaunch(ctx, r); !errors.Is(err, teams.ErrDenied) {
		t.Fatalf("unleased write: %v", err)
	}
	put(t, s, r)
	put(t, s, r)
	entries, err := s.Journal(ctx, r.Key, 0, 10)
	must(t, err)
	if len(entries) != 1 || entries[0].Revision != 1 || entries[0].Fence != 1 {
		t.Fatalf("idempotent journal: %+v", entries)
	}
	conflict := r
	conflict.Digest = "different"
	if err := s.WithLease(ctx, r.Key, func(ctx context.Context) error { return s.PutLaunch(ctx, conflict) }); !errors.Is(err, teams.ErrConflict) {
		t.Fatalf("key rebind: %v", err)
	}
	conflict = r
	conflict.State = teams.MembersReady
	if err := s.WithLease(ctx, r.Key, func(ctx context.Context) error { return s.PutLaunch(ctx, conflict) }); !errors.Is(err, teams.ErrConflict) {
		t.Fatal("skipped steps accepted")
	}
	for _, state := range []teams.LaunchState{teams.Prepared, teams.Launched, teams.MembersReady, teams.RoutingReady} {
		r.State = state
		put(t, s, r)
	}
	entries, err = s.Journal(ctx, r.Key, 1, 2)
	must(t, err)
	if len(entries) != 2 || entries[0].Revision != 2 || entries[1].Revision != 3 || entries[0].Fence <= 1 {
		t.Fatal("journal page/fence incorrect")
	}
	r.State = teams.Planning
	if err := s.WithLease(ctx, r.Key, func(ctx context.Context) error { return s.PutLaunch(ctx, r) }); !errors.Is(err, teams.ErrConflict) {
		t.Fatal("terminal launch resurrected")
	}
	for _, key := range []string{"b", "c", "d"} {
		put(t, s, launch(key))
	}
	abort, err := s.GetLaunch(ctx, "c")
	must(t, err)
	abort.State = teams.Aborting
	put(t, s, abort)
	keys, err := s.Pending(ctx, "c", 2)
	must(t, err)
	if !reflect.DeepEqual(keys, []string{"d", "b"}) {
		t.Fatalf("rotating bounded pending: %v", keys)
	}
	keys, err = s.Pending(ctx, "b", 10)
	must(t, err)
	if !reflect.DeepEqual(keys, []string{"c", "d", "b"}) {
		t.Fatal("aborting launch missing or terminal launch pending")
	}
	abort.State = teams.Failed
	put(t, s, abort)
	keys, err = s.Pending(ctx, "b", 10)
	must(t, err)
	if !reflect.DeepEqual(keys, []string{"d", "b"}) {
		t.Fatal("failed launch pending")
	}
}

func TestLeaseExpiryAndFencingAcrossHandles(t *testing.T) {
	ctx := context.Background()
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	options := teamstore.Options{LeaseDuration: time.Minute, Now: func() time.Time { return time.Unix(0, clock.Load()) }}
	path := t.TempDir() + "/state.db"
	_, a := open(t, path, options)
	_, b := open(t, path, options)
	createRun(t, a, "run-one")
	r := launch("key")
	err := a.WithLease(ctx, r.Key, func(old context.Context) error {
		must(t, a.PutLaunch(old, r))
		blocked, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		defer cancel()
		if err := b.WithLease(blocked, r.Key, func(context.Context) error { t.Error("concurrent live lease admitted"); return nil }); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lease wait: %v", err)
		}
		clock.Add(int64(2 * time.Minute))
		return b.WithLease(ctx, r.Key, func(current context.Context) error {
			next := r
			next.State = teams.Prepared
			must(t, b.PutLaunch(current, next))
			if err := a.PutLaunch(old, r); !errors.Is(err, teamstore.ErrLeaseLost) {
				t.Fatalf("stale holder wrote: %v", err)
			}
			if err := a.Mutate(old, "run-one", func(*teams.Roster) error { t.Error("superseded roster callback"); return nil }); !errors.Is(err, teamstore.ErrLeaseLost) {
				t.Fatalf("superseded roster: %v", err)
			}
			signal := teams.PhaseSignalRecord{RunID: "run-one", PhaseID: "work", Actor: "msg://agent/test/worker", RosterVersion: 1}
			if err := a.RecordSignal(old, signal); !errors.Is(err, teamstore.ErrLeaseLost) {
				t.Fatalf("superseded signal: %v", err)
			}
			if _, err := a.ResolveSignal(old, teams.SignalResolution{RunID: signal.RunID, PhaseID: signal.PhaseID, Actor: signal.Actor, Signaler: signal.Actor, RosterVersion: 1}); !errors.Is(err, teamstore.ErrLeaseLost) {
				t.Fatalf("superseded resolution: %v", err)
			}
			wrong := next
			wrong.Key = "other"
			if err := b.PutLaunch(current, wrong); !errors.Is(err, teams.ErrDenied) {
				t.Fatal("lease authority crossed keys")
			}
			return nil
		})
	})
	if !errors.Is(err, teamstore.ErrLeaseLost) {
		t.Fatalf("expired callback success: %v", err)
	}
	recovered, err := a.GetLaunch(ctx, r.Key)
	must(t, err)
	if recovered.State != teams.Prepared {
		t.Fatal("stale lease clobbered newer state")
	}
	journal, err := b.Journal(ctx, r.Key, 0, 10)
	must(t, err)
	if len(journal) != 2 || journal[1].Fence <= journal[0].Fence {
		t.Fatal("replacement did not advance durable fence")
	}
}

func TestRestartRecoveryAndReadOnly(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/state.db"
	db, s := open(t, path, teamstore.Options{})
	run := createRun(t, s, "run-one")
	must(t, s.Mutate(ctx, run.Run.ID, func(r *teams.Roster) error {
		r.Members = []teams.Member{{ID: "worker", Actor: "msg://agent/test/worker"}}
		return nil
	}))
	signal := teams.PhaseSignalRecord{RunID: run.Run.ID, PhaseID: "work", Actor: "msg://agent/test/worker", RosterVersion: 1}
	must(t, s.RecordSignal(ctx, signal))
	winner, err := s.ResolveSignal(ctx, teams.SignalResolution{RunID: run.Run.ID, PhaseID: "work", Actor: signal.Actor, Signaler: signal.Actor, RosterVersion: 1, Output: "done"})
	must(t, err)
	r := launch("restart")
	put(t, s, r)
	// Simulate a crashed host: a durable lease remains without a callback or release.
	expiry := time.Now().Add(time.Minute).UnixNano()
	_, err = db.DB().ExecContext(ctx, `UPDATE team_launch_leases SET owner='crashed',expires_ns=? WHERE launch_key=?`, expiry, r.Key)
	must(t, err)
	must(t, db.Close())
	_, reopened := open(t, path, teamstore.Options{Now: func() time.Time { return time.Unix(0, expiry+1) }})
	must(t, reopened.WithLease(ctx, r.Key, func(ctx context.Context) error {
		got, err := reopened.GetLaunch(ctx, r.Key)
		if err != nil {
			return err
		}
		got.State = teams.Prepared
		return reopened.PutLaunch(ctx, got)
	}))
	ro, err := store.OpenReadOnly(path)
	must(t, err)
	t.Cleanup(func() { _ = ro.Close() })
	reader, err := teamstore.New(ro.DB(), teamstore.Options{})
	must(t, err)
	got, err := reader.GetRun(ctx, run.Run.ID)
	must(t, err)
	if !reflect.DeepEqual(got, run) {
		t.Fatal("run did not survive reopen")
	}
	_, err = reader.GetDefinition(ctx, "team-one", 1)
	must(t, err)
	roster, err := reader.SnapshotAt(ctx, run.Run.ID, 1)
	must(t, err)
	if len(roster.Members) != 1 {
		t.Fatal("snapshot lost")
	}
	current, err := reader.Snapshot(ctx, run.Run.ID)
	must(t, err)
	if !reflect.DeepEqual(current, roster) {
		t.Fatal("current roster lost")
	}
	signals, err := reader.ListSignals(ctx, run.Run.ID, "work")
	must(t, err)
	if len(signals) != 1 || signals[0] != signal {
		t.Fatal("signal evidence lost")
	}
	resolution, err := reader.GetSignal(ctx, run.Run.ID, "work")
	must(t, err)
	if !reflect.DeepEqual(winner, resolution) {
		t.Fatal("winner lost")
	}
	recovered, err := reader.GetLaunch(ctx, r.Key)
	must(t, err)
	if recovered.State != teams.Prepared {
		t.Fatal("reconciliation state lost")
	}
	entries, err := reader.Journal(ctx, r.Key, 0, 10)
	must(t, err)
	if len(entries) != 2 || entries[1].Fence <= entries[0].Fence {
		t.Fatal("journal/fence lost on crash")
	}
	keys, err := reader.Pending(ctx, "", 10)
	must(t, err)
	if !reflect.DeepEqual(keys, []string{r.Key}) {
		t.Fatal("pending lost")
	}
	authored := definition()
	authored.Version = 2
	if err := reader.PutDefinition(ctx, authored); err == nil {
		t.Fatal("read-only handle wrote definition")
	}
	if err := reader.Mutate(ctx, run.Run.ID, func(*teams.Roster) error { return nil }); err == nil {
		t.Fatal("read-only handle mutated roster")
	}
	// Run creation records metadata only; it never creates a channel or session.
	var count int
	must(t, ro.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions").Scan(&count))
	if count != 0 {
		t.Fatal("storage created a session")
	}
}

func TestLaunchWriteAndJournalRollbackTogether(t *testing.T) {
	ctx := context.Background()
	db, s := open(t, t.TempDir()+"/state.db", teamstore.Options{})
	r := launch("atomic")
	put(t, s, r)
	_, err := db.DB().ExecContext(ctx, `CREATE TRIGGER reject_journal BEFORE INSERT ON team_launch_journal BEGIN SELECT RAISE(ABORT,'journal unavailable'); END`)
	must(t, err)
	r.State = teams.Prepared
	if err := s.WithLease(ctx, r.Key, func(ctx context.Context) error { return s.PutLaunch(ctx, r) }); err == nil {
		t.Fatal("journal failure admitted update")
	}
	current, err := s.GetLaunch(ctx, r.Key)
	must(t, err)
	if current.State != teams.Planning {
		t.Fatal("record committed without journal")
	}
	entries, err := s.Journal(ctx, r.Key, 0, 10)
	must(t, err)
	if len(entries) != 1 {
		t.Fatal("failed journal changed history")
	}
	_, err = db.DB().ExecContext(ctx, `DROP TRIGGER reject_journal`)
	must(t, err)
	put(t, s, r)
	entries, err = s.Journal(ctx, r.Key, 0, 10)
	must(t, err)
	if len(entries) != 2 || entries[1].Revision != 2 {
		t.Fatal("failed transaction consumed revision")
	}
}

func TestLeaseReleasedOnCallbackFailureAndPanic(t *testing.T) {
	ctx := context.Background()
	_, s := open(t, t.TempDir()+"/state.db", teamstore.Options{})
	refusal := errors.New("refused")
	if err := s.WithLease(ctx, "key", func(context.Context) error { return refusal }); !errors.Is(err, refusal) {
		t.Fatal("callback error lost")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic swallowed")
			}
		}()
		_ = s.WithLease(ctx, "key", func(context.Context) error { panic("callback panic") })
	}()
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	must(t, s.WithLease(bounded, "key", func(context.Context) error { return nil }))
}
