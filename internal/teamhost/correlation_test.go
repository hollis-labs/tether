package teamhost_test

import (
	"errors"
	"testing"

	"github.com/hollis-labs/libs/mesh/core/teams"
	"github.com/hollis-labs/tether/internal/teamhost"
)

func TestRunCorrelationPersistsAndIsImmutable(t *testing.T) {
	path := t.TempDir() + "/db"
	db, storage, host := open(t, path, fake())
	record := lookupRecord("correlation")
	var err error
	record.Definition, err = teams.CompileTeam(record.Team, record.Team.Phases)
	must(t, err)
	must(t, storage.PutDefinition(ctx, record.Team))
	must(t, putLookupRecord(storage, record))
	must(t, host.PrepareLaunchCorrelation(ctx, record.Key, "CW-1", "workstream:01M7VQ2QWYB5RZR86W2BWG8PW3"))
	before, task, work, err := teamhost.SessionCorrelation(ctx, db.DB(), record.Intents[0].Request)
	must(t, err)
	actual, err := host.LaunchWorkflow(ctx, record.Key, record.Definition)
	must(t, err)
	if before != actual || task != "CW-1" || work != "workstream:01M7VQ2QWYB5RZR86W2BWG8PW3" {
		t.Fatal("eager member lost correlation")
	}
	// Reopen the database to prove no in-memory request context is required.
	reopened, _, rehost := open(t, path, fake())
	run, task2, work2, err := teamhost.SessionCorrelation(ctx, reopened.DB(), teams.ProvisionRequest{RunID: actual})
	must(t, err)
	if run != actual || task2 != task || work2 != work {
		t.Fatal("join after reopen lost correlation")
	}
	must(t, rehost.PrepareLaunchCorrelation(ctx, record.Key, task, work))
	if err := rehost.PrepareLaunchCorrelation(ctx, record.Key, "different", work); !errors.Is(err, teams.ErrConflict) {
		t.Fatal("metadata replaced", err)
	}
	for _, pair := range [][2]string{{"bad task", "ok"}, {"ok", "workstream:bad"}} {
		if !errors.Is(rehost.PrepareLaunchCorrelation(ctx, "invalid", pair[0], pair[1]), teamhost.ErrInvalidRequest) {
			t.Fatal("invalid accepted")
		}
	}
	run, task, work, err = teamhost.SessionCorrelation(ctx, reopened.DB(), teams.ProvisionRequest{RunID: "old-run"})
	must(t, err)
	if run != "" || task != "" || work != "" {
		t.Fatal("legacy run gained correlation")
	}
}
