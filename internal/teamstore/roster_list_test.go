package teamstore_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/teamstore"
)

func TestRosterListingUsesRetainedExactMembershipAndCursor(t *testing.T) {
	ctx := context.Background()
	_, storage := open(t, t.TempDir()+"/state.db", teamstore.Options{})
	actor := mesh.URN("msg://agent/test/reader")
	for _, item := range []struct {
		id, status string
		kind       mesh.ActorKind
	}{
		{"a", "active", mesh.ActorAgent}, {"b", "released", mesh.ActorAgent},
		{"c", "provisioning", mesh.ActorAgent}, {"d", "active", mesh.ActorUser},
	} {
		createRun(t, storage, item.id)
		must(t, storage.Mutate(ctx, item.id, func(r *teams.Roster) error {
			r.Members = []teams.Member{{ID: "reader", Actor: actor, Kind: item.kind, Status: item.status}}
			return nil
		}))
	}
	before, err := storage.Snapshot(ctx, "a")
	must(t, err)
	page, err := storage.ListRostersForActor(ctx, actor, mesh.ActorAgent, "", 1)
	must(t, err)
	if len(page) != 1 || page[0].RunID != "a" {
		t.Fatal(page)
	}
	next, err := storage.ListRostersForActor(ctx, actor, mesh.ActorAgent, "a", 10)
	must(t, err)
	if len(next) != 1 || next[0].RunID != "b" {
		t.Fatal(next)
	}
	unknown, err := storage.ListRostersForActor(ctx, "msg://agent/test/stranger", mesh.ActorAgent, "", 10)
	must(t, err)
	if len(unknown) != 0 {
		t.Fatal("unrelated actor saw runs", unknown)
	}
	after, err := storage.Snapshot(ctx, "a")
	must(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("read changed durable roster")
	}
}
