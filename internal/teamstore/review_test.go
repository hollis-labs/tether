package teamstore_test

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/substrate/mesh/teams/memory"
	"github.com/hollis-labs/tether/internal/teamstore"
)

func TestNormalizedImmutableRetries(t *testing.T) {
	ctx := context.Background()
	db, s := open(t, t.TempDir()+"/state.db", teamstore.Options{})
	team := definition()
	team.Slots[0].Workspace = map[string]string{}
	must(t, s.PutDefinition(ctx, team))
	must(t, s.PutDefinition(ctx, team))
	// A prior schema omits a zero field which the current schema serializes.
	_, err := db.DB().ExecContext(ctx, `UPDATE team_definitions SET payload=json_remove(payload,'$.slots[0].role')`)
	must(t, err)
	must(t, s.PutDefinition(ctx, team))
	run := teams.TeamRun{ID: "run-one", TeamID: team.ID, TeamVersion: 1, Status: mesh.TaskWorking, Channel: "team/run-one"}
	container, err := s.CreateRun(ctx, run)
	must(t, err)
	if container.Run.Channel != run.Channel || container.SessionGroupID != "team.run-one" {
		t.Fatal("library metadata and group name conflated")
	}
	_, err = db.DB().ExecContext(ctx, `UPDATE team_runs SET payload=json_remove(payload,'$.channel')`)
	must(t, err)
	run.Channel = ""
	_, err = s.CreateRun(ctx, run)
	must(t, err)
	record := launch("normalized")
	record.Team.Slots[0].Workspace = map[string]string{}
	record.Intents[0].Slot.Workspace = map[string]string{}
	put(t, s, record)
	put(t, s, record)
	entries, err := s.Journal(ctx, record.Key, 0, 10)
	must(t, err)
	if len(entries) != 1 {
		t.Fatal("normalized retry appended journal")
	}
	record.State = teams.Prepared
	put(t, s, record)
}

func TestExpiredLeaseFencesAllWriters(t *testing.T) {
	ctx := context.Background()
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	options := teamstore.Options{LeaseDuration: time.Minute, Now: func() time.Time { return time.Unix(0, clock.Load()) }}
	_, s := open(t, t.TempDir()+"/state.db", options)
	createRun(t, s, "run-one")
	record := launch("lease")
	signal := teams.PhaseSignalRecord{RunID: "run-one", PhaseID: "work", Actor: "msg://agent/test/worker", RosterVersion: 1}
	resolution := teams.SignalResolution{RunID: signal.RunID, PhaseID: signal.PhaseID, Actor: signal.Actor, Signaler: signal.Actor, RosterVersion: 1}
	err := s.WithLease(ctx, record.Key, func(leased context.Context) error {
		clock.Add(int64(2 * time.Minute))
		writes := []func() error{
			func() error { return s.PutLaunch(leased, record) },
			func() error {
				return s.Mutate(leased, "run-one", func(*teams.Roster) error { t.Error("expired mutation invoked callback"); return nil })
			},
			func() error { return s.RecordSignal(leased, signal) },
			func() error { _, err := s.ResolveSignal(leased, resolution); return err },
		}
		for _, write := range writes {
			if err := write(); !errors.Is(err, teamstore.ErrLeaseLost) {
				t.Errorf("expired writer: %v", err)
			}
		}
		return nil
	})
	if !errors.Is(err, teamstore.ErrLeaseLost) {
		t.Fatalf("expiry: %v", err)
	}
	if _, err := s.Snapshot(ctx, "run-one"); !errors.Is(err, teams.ErrNotFound) {
		t.Fatal("expired roster persisted")
	}
	signals, err := s.ListSignals(ctx, "run-one", "work")
	must(t, err)
	if len(signals) != 0 {
		t.Fatal("expired signal persisted")
	}
	if _, err := s.GetSignal(ctx, "run-one", "work"); !errors.Is(err, teams.ErrNotFound) {
		t.Fatal("expired resolution persisted")
	}
	must(t, s.RecordSignal(ctx, signal)) // Unleased host writes remain permitted.
	missing := signal
	missing.RunID = "missing"
	if err := s.RecordSignal(ctx, missing); !errors.Is(err, teams.ErrNotFound) {
		t.Fatal("orphan signal admitted")
	}
	resolution.RunID = "missing"
	if _, err := s.ResolveSignal(ctx, resolution); !errors.Is(err, teams.ErrNotFound) {
		t.Fatal("orphan resolution admitted")
	}
}

func TestLateReleasePreservesReplacementLease(t *testing.T) {
	ctx := context.Background()
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	options := teamstore.Options{LeaseDuration: time.Minute, Now: func() time.Time { return time.Unix(0, clock.Load()) }}
	path := t.TempDir() + "/state.db"
	_, a := open(t, path, options)
	_, b := open(t, path, options)
	acquired := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- a.WithLease(ctx, "key", func(context.Context) error { close(acquired); <-release; return nil })
	}()
	<-acquired
	clock.Add(int64(2 * time.Minute))
	must(t, b.WithLease(ctx, "key", func(current context.Context) error {
		close(release)
		if err := <-finished; !errors.Is(err, teamstore.ErrLeaseLost) {
			t.Errorf("stale callback: %v", err)
		}
		return b.PutLaunch(current, launch("key"))
	}))
}

func TestExpiryDuringWriteRollsBack(t *testing.T) {
	ctx := context.Background()
	db, s := open(t, t.TempDir()+"/state.db", teamstore.Options{})
	record := launch("atomic-expiry")
	put(t, s, record)
	// Expire the lease after the entry check, while the journal is written.
	_, err := db.DB().ExecContext(ctx, `CREATE TRIGGER expire_lease AFTER INSERT ON team_launch_journal BEGIN UPDATE team_launch_leases SET expires_ns=0 WHERE launch_key=NEW.launch_key; END`)
	must(t, err)
	record.State = teams.Prepared
	if err := s.WithLease(ctx, record.Key, func(leased context.Context) error { return s.PutLaunch(leased, record) }); !errors.Is(err, teamstore.ErrLeaseLost) {
		t.Fatalf("end check: %v", err)
	}
	current, err := s.GetLaunch(ctx, record.Key)
	must(t, err)
	entries, err := s.Journal(ctx, record.Key, 0, 10)
	must(t, err)
	if current.State != teams.Planning || len(entries) != 1 {
		t.Fatal("expiry committed record or journal")
	}
	createRun(t, s, "run-one")
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	fenced, err := teamstore.New(db.DB(), teamstore.Options{LeaseDuration: time.Minute, Now: func() time.Time { return time.Unix(0, clock.Load()) }})
	must(t, err)
	if err := fenced.WithLease(ctx, "roster", func(leased context.Context) error {
		return fenced.Mutate(leased, "run-one", func(r *teams.Roster) error {
			clock.Add(int64(2 * time.Minute))
			r.Members = []teams.Member{{ID: "worker"}}
			return nil
		})
	}); !errors.Is(err, teamstore.ErrLeaseLost) {
		t.Fatalf("mutation expiry: %v", err)
	}
	if _, err := s.Snapshot(ctx, "run-one"); !errors.Is(err, teams.ErrNotFound) {
		t.Fatal("expired callback committed roster")
	}
}

type workflowStore struct {
	*memory.Host
	storage *teamstore.Store
}

func (w workflowStore) LaunchWorkflow(ctx context.Context, key string, definition teams.WorkflowDefinition) (string, error) {
	id, err := w.Host.LaunchWorkflow(ctx, key, definition)
	if err != nil {
		return "", err
	}
	record, err := w.storage.GetLaunch(ctx, key)
	if err != nil {
		return "", err
	}
	_, err = w.storage.CreateRun(ctx, teams.TeamRun{ID: id, TeamID: record.Team.ID, TeamVersion: record.Team.Version, Channel: "team/" + id, Status: mesh.TaskWorking})
	return id, err
}

func TestLauncherAgainstDurableStore(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/state.db"
	db, s := open(t, path, teamstore.Options{})
	must(t, s.PutDefinition(ctx, definition()))
	host := memory.New()
	launcher := teams.Launcher{Definitions: s, Roster: s, Ledger: s, Provisioner: host, Workflows: workflowStore{Host: host, storage: s}, Routing: host, Clock: host, IDs: host, Defaults: limits()}
	request := teams.LaunchRequest{Key: "launch", TeamID: "team-one", Version: 1, Limits: limits()}
	run, err := launcher.Launch(ctx, request)
	must(t, err)
	roster, err := s.Snapshot(ctx, run.ID)
	must(t, err)
	if len(roster.Members) != 1 || roster.Members[0].Actor == "" || roster.Version != 1 {
		t.Fatalf("roster: %+v", roster)
	}
	record, err := s.GetLaunch(ctx, request.Key)
	must(t, err)
	if record.State != teams.RoutingReady || !reflect.DeepEqual(record.Run, run) {
		t.Fatal("launch did not finish")
	}
	container, err := s.GetRun(ctx, run.ID)
	must(t, err)
	if !reflect.DeepEqual(container.Run, run) {
		t.Fatal("run and journal metadata disagree")
	}
	group, err := db.GetSessionGroup(container.SessionGroupID)
	must(t, err)
	if group.WorkflowID != run.ID {
		t.Fatal("workflow container missing")
	}
	must(t, db.Close())
	_, reopened := open(t, path, teamstore.Options{})
	launcher.Definitions = reopened
	launcher.Roster = reopened
	launcher.Ledger = reopened
	launcher.Workflows = workflowStore{Host: host, storage: reopened}
	again, err := launcher.Launch(ctx, request)
	must(t, err)
	after, err := reopened.Snapshot(ctx, run.ID)
	must(t, err)
	if !reflect.DeepEqual(again, run) || !reflect.DeepEqual(after, roster) || len(host.Provisioned()) != 1 {
		t.Fatal("reopened retry changed completed launch")
	}
}
