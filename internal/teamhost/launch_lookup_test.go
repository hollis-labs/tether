package teamhost_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/teamstore"
)

func lookupRecord(key string) teams.LaunchRecord {
	slot := definition().Slots[0]
	req := teams.ProvisionRequest{IdempotencyKey: key + "-member", MemberID: key + "-member", Slot: slot, Limits: limits()}
	return teams.LaunchRecord{Key: key, Digest: "digest", State: teams.Planning, Team: definition(), Limits: limits(), Deadline: time.Now().Add(time.Minute), Intents: []teams.MemberIntent{{Key: req.IdempotencyKey, MemberID: req.MemberID, Slot: slot, Request: req}}}
}
func putLookupRecord(s *teamstore.Store, r teams.LaunchRecord) error {
	return s.WithLease(ctx, r.Key, func(leased context.Context) error { return s.PutLaunch(leased, r) })
}
func TestLaunchLookupAtomicFailures(t *testing.T) {
	f := fake()
	db, s, _ := open(t, t.TempDir()+"/db", f)
	_, err := db.DB().Exec(`ALTER TABLE team_host_launch_intents RENAME TO blocked_lookup`)
	must(t, err)
	r := lookupRecord("lookup-fails")
	if err = putLookupRecord(s, r); err == nil {
		t.Fatal("lookup failure did not roll back launch")
	}
	if _, err = s.GetLaunch(ctx, r.Key); !errors.Is(err, teams.ErrNotFound) {
		t.Fatal("lookup failure retained launch", err)
	}
	journal, err := s.Journal(ctx, r.Key, 0, 10)
	must(t, err)
	if len(journal) != 0 {
		t.Fatal("lookup failure retained journal")
	}
	_, err = db.DB().Exec(`ALTER TABLE blocked_lookup RENAME TO team_host_launch_intents`)
	must(t, err)
	must(t, putLookupRecord(s, lookupRecord("seed")))
	_, err = db.DB().Exec(`CREATE UNIQUE INDEX inject_launch_failure ON team_launches(digest)`)
	must(t, err)
	r = lookupRecord("launch-fails")
	err = putLookupRecord(s, r)
	if err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatal("launch write fault did not fire", err)
	}
	var present bool
	must(t, db.DB().QueryRow(`SELECT EXISTS(SELECT 1 FROM team_host_launch_intents WHERE intent_key=?)`, r.Intents[0].Key).Scan(&present))
	if present {
		t.Fatal("failed launch write retained lookup")
	}
}
func TestLaunchLookupProvisionAfterReopenAndIdenticalRetry(t *testing.T) {
	f := fake()
	path := t.TempDir() + "/db"
	db, s, _ := open(t, path, f)
	r := lookupRecord("reopen")
	must(t, putLookupRecord(s, r))
	must(t, putLookupRecord(s, r))
	must(t, db.Close())
	db, s, h := open(t, path, f)
	m, err := h.Provision(ctx, r.Intents[0].Request)
	must(t, err)
	if m.ID != r.Intents[0].MemberID || m.SessionID == "" {
		t.Fatal("lookup provenance lost after reopen")
	}
	_, err = db.DB().Exec(`DELETE FROM team_host_launch_intents WHERE intent_key=?`, r.Intents[0].Key)
	must(t, err)
	must(t, putLookupRecord(s, r))
	var key string
	must(t, db.DB().QueryRow(`SELECT launch_key FROM team_host_launch_intents WHERE intent_key=?`, r.Intents[0].Key).Scan(&key))
	if key != r.Key {
		t.Fatal("identical retry did not repair lookup")
	}
	journal, err := s.Journal(ctx, r.Key, 0, 10)
	must(t, err)
	if len(journal) != 1 {
		t.Fatal("lookup repair appended journal")
	}
}
func TestPlainLaunchWriterAndLookupBackfill(t *testing.T) {
	f := fake()
	db, _, _ := open(t, t.TempDir()+"/db", f)
	plain, err := teamstore.New(db.DB(), teamstore.Options{})
	must(t, err)
	r := lookupRecord("plain")
	must(t, putLookupRecord(plain, r))
	var present bool
	must(t, db.DB().QueryRow(`SELECT EXISTS(SELECT 1 FROM team_host_launch_intents WHERE intent_key=?)`, r.Intents[0].Key).Scan(&present))
	if present {
		t.Fatal("plain store unexpectedly wrote host lookup")
	}
	// Execute the retained migration backfill over this genuine plain-store row.
	migration, err := os.ReadFile("../store/migrations/0051_team_host.sql")
	must(t, err)
	statement := strings.Split(strings.Split(string(migration), "INSERT OR IGNORE INTO team_host_launch_intents")[1], ";")[0]
	_, err = db.DB().Exec("INSERT OR IGNORE INTO team_host_launch_intents" + statement)
	must(t, err)
	_, err = db.DB().Exec("INSERT OR IGNORE INTO team_host_launch_intents" + statement)
	must(t, err)
	var key string
	must(t, db.DB().QueryRow(`SELECT launch_key FROM team_host_launch_intents WHERE intent_key=?`, r.Intents[0].Key).Scan(&key))
	if key != r.Key {
		t.Fatal("backfill omitted plain-store record")
	}
}
