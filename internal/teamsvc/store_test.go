package teamsvc

import (
	"context"
	"testing"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamstore"
)

type storedRuns struct{ storage *teamstore.Store }

func (r storedRuns) GetRun(ctx context.Context, id string) (teams.TeamRun, error) {
	container, err := r.storage.GetRun(ctx, id)
	return container.Run, err
}

type storedWorkflows struct {
	storage *teamstore.Store
	team    teams.Team
}

func (w storedWorkflows) LaunchWorkflow(ctx context.Context, key string, _ teams.WorkflowDefinition) (string, error) {
	id := stableKey(key)
	_, err := w.storage.CreateRun(ctx, teams.TeamRun{ID: id, TeamID: w.team.ID, TeamVersion: w.team.Version, Channel: "team/" + id, Status: mesh.TaskWorking})
	return id, err
}
func (w storedWorkflows) FailWorkflow(context.Context, string, string) error { return nil }

func TestFormationWithDurableLibraryStores(t *testing.T) {
	f := fixtureNew(t)
	path := t.TempDir() + "/state.db"
	db, err := store.Open(path)
	must(t, err)
	t.Cleanup(func() { _ = db.Close() })
	storage, err := teamstore.New(db.DB(), teamstore.Options{})
	must(t, err)
	d := f.svc.deps
	d.Definitions, d.Roster, d.Ledger, d.Signals = storage, storage, storage, storage
	d.Runs = storedRuns{storage}
	d.Workflows = storedWorkflows{storage, f.team}
	service, err := New(d)
	must(t, err)
	ctx := caller(verified(ownerURN))
	req := FormRequest{Key: "durable", Team: definition()}
	first, err := service.Form(ctx, req)
	must(t, err)
	run, err := storage.GetRun(ctx, first.Run.ID)
	must(t, err)
	if run.SessionGroupID != "team."+first.Run.ID || run.Run.Channel != "team/"+first.Run.ID {
		t.Fatal("library channel metadata changed", run)
	}
	roster, err := storage.Snapshot(ctx, first.Run.ID)
	must(t, err)
	if len(roster.Members) != 1 || roster.Members[0].Actor != ownerURN || roster.Members[0].Governance != teams.Owner {
		t.Fatal("caller not owner", roster)
	}
	// Reopen the actual stores; only the reference journal/ports are test-local.
	must(t, db.Close())
	db, err = store.Open(path)
	must(t, err)
	storage, err = teamstore.New(db.DB(), teamstore.Options{})
	must(t, err)
	d.Definitions, d.Roster, d.Ledger, d.Signals = storage, storage, storage, storage
	d.Runs = storedRuns{storage}
	d.Workflows = storedWorkflows{storage, f.team}
	service, err = New(d)
	must(t, err)
	replay, err := service.Form(ctx, req)
	must(t, err)
	if replay.Run.ID != first.Run.ID || len(f.host.Provisioned()) != 1 {
		t.Fatal("reopened formation duplicated")
	}
	_, err = service.Dissolve(ctx, RunRequest{Key: "end", RunID: first.Run.ID})
	must(t, err)
	roster, err = storage.Snapshot(ctx, first.Run.ID)
	must(t, err)
	if roster.Members[0].Status != "released" {
		t.Fatal("durable roster not ended", roster)
	}
}
