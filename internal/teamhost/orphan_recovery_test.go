package teamhost_test

import (
	"errors"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mesh/core/teams"
	"github.com/hollis-labs/tether/internal/teamhost"
)

func TestExpiredOrphanJournalsCleanupOnceAndPreservesLiveFormation(t *testing.T) {
	f := fake()
	path := t.TempDir() + "/db"
	db, s, h := open(t, path, f)
	expired := lookupRecord("expired")
	expired.Deadline = time.Now().Add(-time.Hour)
	must(t, putLookupRecord(s, expired))
	live := lookupRecord("live")
	must(t, putLookupRecord(s, live))
	must(t, h.ReconcileExpiredFormations(ctx, 10))
	got, err := s.GetLaunch(ctx, expired.Key)
	must(t, err)
	if got.State != teams.Failed || !got.Intents[0].Cleaned {
		t.Fatalf("not abandoned: %+v", got)
	}
	var tombstone string
	var cleaned bool
	must(t, db.DB().QueryRow(`SELECT tombstone,cleaned FROM team_host_intents WHERE intent_key=?`, expired.Intents[0].Key).Scan(&tombstone, &cleaned))
	if tombstone != "retire" || !cleaned {
		t.Fatal(tombstone, cleaned)
	}
	journal, err := s.Journal(ctx, expired.Key, 0, 10)
	must(t, err)
	if len(journal) != 4 || journal[1].Record.State != teams.Aborting || !journal[2].Record.Intents[0].Cleaned {
		t.Fatalf("missing abort/cleanup journal: %+v", journal)
	}
	must(t, db.Close())
	_, s, h = open(t, path, f)
	must(t, h.ReconcileExpiredFormations(ctx, 10))
	if len(f.stopOrder) != 1 || f.stopOrder[0] != expired.Intents[0].Key {
		t.Fatal("repeated or foreign cleanup", f.stopOrder)
	}
	got, err = s.GetLaunch(ctx, live.Key)
	must(t, err)
	if got.State != teams.Planning || len(f.sessions) != 0 {
		t.Fatal("live formation advanced")
	}
	journal, err = s.Journal(ctx, expired.Key, 0, 10)
	must(t, err)
	if len(journal) != 4 {
		t.Fatal("duplicate journal")
	}
}

func TestOrphanRecoveryProtectsLostWorkflowAcknowledgement(t *testing.T) {
	f := fake()
	_, s, h := open(t, t.TempDir()+"/db", f)
	r := lookupRecord("committed")
	r.Deadline = time.Now().Add(-time.Hour)
	var err error
	r.Definition, err = teams.CompileTeam(r.Team, r.Team.Phases)
	must(t, err)
	must(t, s.PutDefinition(ctx, r.Team))
	must(t, putLookupRecord(s, r))
	// Workflow exists, but its ID never reached the formation record.
	run, err := h.LaunchWorkflow(ctx, r.Key, r.Definition)
	must(t, err)
	must(t, h.ReconcileExpiredFormations(ctx, 10))
	got, err := h.GetRun(ctx, run)
	must(t, err)
	if got.Status.Terminal() || len(f.stopOrder) != 0 {
		t.Fatal("committed run cleaned", got)
	}
}

func TestOrphanCleanupFailureBackoffAndRestartResume(t *testing.T) {
	f := fake()
	path := t.TempDir() + "/db"
	db, s, h := open(t, path, f)
	r := lookupRecord("retry")
	r.Deadline = time.Now().Add(-time.Hour)
	must(t, putLookupRecord(s, r))
	failure := errors.New("stop unavailable")
	calls := 0
	f.before = func(op, key string) error {
		if op == "stop" {
			calls++
			return failure
		}
		return nil
	}
	if err := h.ReconcileExpiredFormations(ctx, 10); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	must(t, h.ReconcileExpiredFormations(ctx, 10))
	if calls != 1 {
		t.Fatal("cleanup retried before due", calls)
	}
	must(t, db.Close())
	db, s, h = open(t, path, f)
	must(t, h.ReconcileExpiredFormations(ctx, 10))
	if calls != 1 {
		t.Fatal("restart lost backoff")
	}
	f.before = nil
	// Inject clock through the existing host option; no test sleeps.
	h, err := teamhost.New(db.DB(), s, teamhost.Ports{Enroller: f, Sessions: f, Messenger: f, Channels: f}, teamhost.Options{Now: func() time.Time { return time.Now().Add(time.Minute) }})
	must(t, err)
	must(t, h.ReconcileExpiredFormations(ctx, 10))
	got, err := s.GetLaunch(ctx, r.Key)
	must(t, err)
	if got.State != teams.Failed || !got.Intents[0].Cleaned {
		t.Fatal("cleanup not resumed", got)
	}
}
