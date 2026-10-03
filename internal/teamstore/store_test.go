package teamstore_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamstore"
)

func definition() teams.Team {
	return teams.Team{ID: "team-one", Name: "One team", Version: 1,
		Slots:  []teams.Slot{{Name: "worker", Definition: mesh.DefinitionRef{ID: "worker", Revision: "r1"}, Resolution: teams.Fresh, Activation: teams.Singleton, Min: 1, Max: 1}},
		Phases: []teams.Phase{{ID: "work", Kind: "flex", ActiveSlots: []string{"worker"}, OwnerSlot: "worker", ExitTrigger: teams.Trigger{Kind: "event", Spec: map[string]string{"event": "done"}}}}, Policy: teams.Policy{Spawn: limits()}}
}
func limits() mesh.Limits {
	return mesh.Limits{MaxDepth: 3, MaxChildren: 8, FanOut: 8, Budget: 100, Timeout: time.Minute}
}
func open(t *testing.T, path string, options teamstore.Options) (*store.Store, *teamstore.Store) {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	storage, err := teamstore.New(db.DB(), options)
	if err != nil {
		t.Fatal(err)
	}
	return db, storage
}
func createRun(t *testing.T, s *teamstore.Store, id string) teamstore.RunContainer {
	t.Helper()
	ctx := context.Background()
	if err := s.PutDefinition(ctx, definition()); err != nil {
		t.Fatal(err)
	}
	result, err := s.CreateRun(ctx, teams.TeamRun{ID: id, TeamID: "team-one", TeamVersion: 1, Status: mesh.TaskWorking})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestDefinitionsAndRunContainer(t *testing.T) {
	ctx := context.Background()
	db, s := open(t, t.TempDir()+"/state.db", teamstore.Options{})
	t.Run("immutable revisions and detached reads", func(t *testing.T) {
		authored := definition()
		must(t, s.PutDefinition(ctx, authored))
		authored.Slots[0].Name = "caller-mutated"
		got, err := s.GetDefinition(ctx, "team-one", 1)
		must(t, err)
		if got.Slots[0].Name != "worker" {
			t.Fatal("caller mutation changed definition")
		}
		got.Slots[0].Workspace = map[string]string{"unsafe": "changed"}
		again, err := s.GetDefinition(ctx, "team-one", 1)
		must(t, err)
		if again.Slots[0].Workspace != nil {
			t.Fatal("snapshot mutation changed definition")
		}
		authored = definition()
		authored.Name = "Different"
		if err := s.PutDefinition(ctx, authored); !errors.Is(err, teams.ErrConflict) {
			t.Fatalf("revision rewrite: %v", err)
		}
		authored.Version = 2
		must(t, s.PutDefinition(ctx, authored))
		original, err := s.GetDefinition(ctx, "team-one", 1)
		must(t, err)
		if original.Name != "One team" {
			t.Fatal("new revision changed prior version")
		}
	})
	t.Run("one owned session group and canonical channel", func(t *testing.T) {
		run := createRun(t, s, "run-one")
		group, err := db.GetSessionGroup(run.SessionGroupID)
		must(t, err)
		if group.WorkflowID != run.Run.ID || run.SessionGroupID != "team.run-one" {
			t.Fatal("run/group/channel ownership missing")
		}
		again := createRun(t, s, "run-one")
		if !reflect.DeepEqual(again, run) {
			t.Fatal("identical create did not recover run")
		}
		changed := run.Run
		changed.TeamVersion = 2
		if _, err := s.CreateRun(ctx, changed); !errors.Is(err, teams.ErrConflict) {
			t.Fatalf("run definition changed: %v", err)
		}
		must(t, db.CreateSessionGroup(store.SessionGroupRow{ID: "team.collision", Name: "other", CreatedAt: "created", UpdatedAt: "updated"}))
		if _, err := s.CreateRun(ctx, teams.TeamRun{ID: "collision", TeamID: "team-one", TeamVersion: 1, Status: mesh.TaskWorking}); !errors.Is(err, teams.ErrConflict) {
			t.Fatal("run adopted an unrelated group")
		}
		if _, err := s.GetRun(ctx, "collision"); !errors.Is(err, teams.ErrNotFound) {
			t.Fatal("group collision partially created run")
		}
	})
	t.Run("missing definitions and invalid channel names", func(t *testing.T) {
		for _, id := range []string{"", "run/child", strings.Repeat("a", 60)} {
			if _, err := teamstore.ChannelName(id); err == nil {
				t.Fatalf("invalid channel accepted: %q", id)
			}
		}
		if _, err := s.CreateRun(ctx, teams.TeamRun{ID: "missing-definition", TeamID: "missing", TeamVersion: 1, Status: mesh.TaskWorking}); !errors.Is(err, teams.ErrNotFound) {
			t.Fatalf("missing pin admitted: %v", err)
		}
		if _, err := db.GetSessionGroup("team.missing-definition"); err == nil {
			t.Fatal("failed run left group")
		}
	})
}

func TestRosterTransactionsAcrossDatabaseHandles(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/state.db"
	_, a := open(t, path, teamstore.Options{})
	_, b := open(t, path, teamstore.Options{})
	createRun(t, a, "run-one")
	must(t, a.Mutate(ctx, "run-one", func(r *teams.Roster) error { r.Members = []teams.Member{{ID: "initial", Status: "active"}}; return nil }))
	initial, err := a.Snapshot(ctx, "run-one")
	must(t, err)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			host := a
			if i%2 == 1 {
				host = b
			}
			if err := host.Mutate(ctx, "run-one", func(r *teams.Roster) error {
				r.Members = append(r.Members, teams.Member{ID: fmt.Sprint(i), Status: "active"})
				return nil
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	current, err := a.Snapshot(ctx, "run-one")
	must(t, err)
	if len(current.Members) != 13 || current.Version != initial.Version+12 {
		t.Fatal("concurrent mutations lost updates")
	}
	retained, err := b.SnapshotAt(ctx, "run-one", initial.Version)
	must(t, err)
	if !reflect.DeepEqual(initial, retained) {
		t.Fatal("immutable roster snapshot changed")
	}
	retained.Members[0].Status = "changed"
	retainedAgain, err := a.SnapshotAt(ctx, "run-one", initial.Version)
	must(t, err)
	if retainedAgain.Members[0].Status != "active" {
		t.Fatal("read snapshot escaped isolation")
	}
	sentinel := errors.New("callback refused")
	if err := a.Mutate(ctx, "run-one", func(r *teams.Roster) error { r.Members = nil; return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("callback refusal: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	if err := a.Mutate(canceled, "run-one", func(r *teams.Roster) error { r.Members = nil; cancel(); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled mutation: %v", err)
	}
	after, err := a.Snapshot(ctx, "run-one")
	must(t, err)
	if !reflect.DeepEqual(current, after) {
		t.Fatal("failed mutation committed roster")
	}
	if _, err := a.SnapshotAt(ctx, "run-one", after.Version+1); !errors.Is(err, teams.ErrNotFound) {
		t.Fatal("failed mutation retained snapshot")
	}
	if err := a.Mutate(ctx, "run-one", func(r *teams.Roster) error { r.Version++; return nil }); !errors.Is(err, teams.ErrConflict) {
		t.Fatal("callback controlled roster version")
	}
	if err := a.Mutate(ctx, "unknown", func(*teams.Roster) error { return nil }); !errors.Is(err, teams.ErrNotFound) {
		t.Fatal("unknown run got orphaned roster")
	}
}

func TestSignalsFirstWinsAndOrder(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/state.db"
	_, a := open(t, path, teamstore.Options{})
	_, b := open(t, path, teamstore.Options{})
	createRun(t, a, "run-one")
	first := teams.PhaseSignalRecord{RunID: "run-one", PhaseID: "work", Actor: "msg://agent/test/first", RosterVersion: 1}
	second := first
	second.Actor = "msg://agent/test/second"
	must(t, a.RecordSignal(ctx, first))
	must(t, b.RecordSignal(ctx, second))
	retry := first
	retry.RosterVersion = 2
	must(t, b.RecordSignal(ctx, retry))
	signals, err := a.ListSignals(ctx, "run-one", "work")
	must(t, err)
	if len(signals) != 2 || signals[0] != first || signals[1] != second {
		t.Fatal("signal retry changed first evidence or ordering")
	}
	var wg sync.WaitGroup
	results := make(chan teams.SignalResolution, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			host := a
			if i%2 == 1 {
				host = b
			}
			result, err := host.ResolveSignal(ctx, teams.SignalResolution{RunID: "run-one", PhaseID: "work", Actor: mesh.URN(fmt.Sprintf("msg://agent/test/worker-%d", i)), Signaler: first.Actor, RosterVersion: 1, MemberIDs: []string{"first"}, Output: fmt.Sprint(i)})
			if err != nil {
				t.Error(err)
				return
			}
			results <- result
		}()
	}
	wg.Wait()
	close(results)
	winner, err := a.GetSignal(ctx, "run-one", "work")
	must(t, err)
	for result := range results {
		if !reflect.DeepEqual(winner, result) {
			t.Fatal("more than one phase resolution won")
		}
	}
	winner.MemberIDs[0] = "mutation"
	again, err := b.GetSignal(ctx, "run-one", "work")
	must(t, err)
	if again.MemberIDs[0] != "first" {
		t.Fatal("resolution returned live state")
	}
}
