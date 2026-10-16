package teamhost_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/mesh/core/teams"
	"github.com/hollis-labs/tether/internal/teamhost"
)

func TestInitialRunIDRequiresExactDurableIntent(t *testing.T) {
	db, storage, host := open(t, t.TempDir()+"/db", fake())
	record := lookupRecord("boot-context")
	var err error
	record.Definition, err = teams.CompileTeam(record.Team, record.Team.Phases)
	must(t, err)
	must(t, storage.PutDefinition(ctx, record.Team))
	must(t, putLookupRecord(storage, record))
	req := record.Intents[0].Request
	before, err := teamhost.InitialRunID(ctx, db.DB(), req)
	must(t, err)
	actual, err := host.LaunchWorkflow(ctx, record.Key, record.Definition)
	must(t, err)
	if before != actual {
		t.Fatalf("boot ID %q differs from workflow %q", before, actual)
	}
	for _, change := range []func(*teams.ProvisionRequest){
		func(r *teams.ProvisionRequest) { r.IdempotencyKey = "unknown" },
		func(r *teams.ProvisionRequest) { r.MemberID = "different" },
		func(r *teams.ProvisionRequest) { r.Parent = "parent" },
		func(r *teams.ProvisionRequest) { r.RunID = "claimed-run" },
		func(r *teams.ProvisionRequest) { r.Slot.Workspace = map[string]string{"mission": "changed"} },
	} {
		bad := req
		change(&bad)
		got, err := teamhost.InitialRunID(ctx, db.DB(), bad)
		if got != "" || !errors.Is(err, teamhost.ErrInvalidRequest) || !strings.Contains(err.Error(), "boot context: run id missing") {
			t.Fatalf("unproven intent accepted: id=%q error=%v", got, err)
		}
	}
}
