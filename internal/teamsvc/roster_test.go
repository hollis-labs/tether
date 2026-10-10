package teamsvc

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamstore"
)

func TestRosterReadRefusesUnverifiedAndUnrelatedPrincipals(t *testing.T) {
	f := fixtureNew(t)
	for _, item := range []struct {
		principal Principal
		want      error
	}{
		{Principal{}, ErrUnauthenticated}, {verified("msg://agent/test/stranger"), ErrNotFound},
		{Principal{ID: LocalOperator, Kind: mesh.ActorUser, LocalOperator: true}, ErrNotFound},
		{Principal{ID: ownerURN, Kind: mesh.ActorUser, Verified: true}, ErrUnauthenticated},
	} {
		_, err := f.svc.ListRoster(caller(item.principal), RosterRequest{RunID: "run"})
		if !errors.Is(err, item.want) {
			t.Fatalf("got %v, want %v", err, item.want)
		}
	}
	before, err := f.host.Snapshot(context.Background(), "run")
	must(t, err)
	view, err := f.svc.ListRoster(caller(verified(ownerURN)), RosterRequest{RunID: "run"})
	must(t, err)
	encoded, err := json.Marshal(view)
	must(t, err)
	if len(view.Runs) != 1 || len(view.Runs[0].Members) != 3 || strings.Contains(string(encoded), "session") || strings.Contains(string(encoded), "intent") {
		t.Fatal(string(encoded))
	}
	after, err := f.host.Snapshot(context.Background(), "run")
	must(t, err)
	if !reflect.DeepEqual(before, after) || len(f.journal.records) != 0 {
		t.Fatal("listing mutated roster or journal")
	}
	must(t, f.host.Mutate(context.Background(), "run", func(r *teams.Roster) error { r.Members[0].Status = "released"; return nil }))
	_, err = f.svc.ListRoster(caller(verified(ownerURN)), RosterRequest{RunID: "run"})
	must(t, err)
}

func TestRosterServicePagesActualStoredMembership(t *testing.T) {
	f := fixtureNew(t)
	db, err := store.Open(t.TempDir() + "/state.db")
	must(t, err)
	t.Cleanup(func() { _ = db.Close() })
	storage, err := teamstore.New(db.DB(), teamstore.Options{})
	must(t, err)
	must(t, storage.PutDefinition(context.Background(), f.team))
	for _, id := range []string{"a", "b", "hidden"} {
		_, err = storage.CreateRun(context.Background(), teams.TeamRun{ID: id, TeamID: f.team.ID, TeamVersion: 1, Status: mesh.TaskWorking})
		must(t, err)
		actor := ownerURN
		if id == "hidden" {
			actor = peerURN
		}
		must(t, storage.Mutate(context.Background(), id, func(r *teams.Roster) error {
			r.Members = []teams.Member{{ID: "member", Actor: actor, Kind: mesh.ActorAgent, Status: "released", Intent: &teams.ProvisionRequest{IdempotencyKey: "private-receipt"}}}
			return nil
		}))
	}
	d := f.svc.deps
	d.Roster, d.Runs = storage, storedRuns{storage}
	svc, err := New(d)
	must(t, err)
	first, err := svc.ListRoster(caller(verified(ownerURN)), RosterRequest{Limit: 1})
	must(t, err)
	if len(first.Runs) != 1 || first.Runs[0].Run.ID != "a" || first.NextAfter != "a" {
		t.Fatal(first)
	}
	next, err := svc.ListRoster(caller(verified(ownerURN)), RosterRequest{After: first.NextAfter, Limit: 1})
	must(t, err)
	if len(next.Runs) != 1 || next.Runs[0].Run.ID != "b" || next.NextAfter != "" {
		t.Fatal(next)
	}
}
