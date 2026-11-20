package teamstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/libs/mesh/core/teams"
	"github.com/hollis-labs/tether/internal/teamstore"
)

func TestReconcileDissolvedRunsProofAndPreservation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	db, s := open(t, path, teamstore.Options{})
	for _, tc := range []struct {
		id, status string
		removed    bool
		want       bool
	}{
		{"ended", "stopped", true, true}, {"released", "released", true, true},
		{"active", "active", true, false}, {"provisioning", "provisioning", true, false},
		{"stopping", "stopping", true, false}, {"unknown", "", true, false},
		{"routing-live", "stopped", false, false}, {"empty-roster", "empty", true, false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			createRun(t, s, tc.id)
			must(t, s.Mutate(ctx, tc.id, func(r *teams.Roster) error {
				if tc.status != "empty" {
					r.Members = []teams.Member{{ID: "owner", Actor: "msg://service/local/owner", Governance: teams.Owner, Status: tc.status}}
				}
				return nil
			}))
			_, err := db.DB().Exec(`INSERT INTO team_host_routing(run_id,install_key,payload,channel,removed) VALUES(?,?,'{}','channel',?)`, tc.id, tc.id, tc.removed)
			must(t, err)
			_, err = db.DB().Exec(`UPDATE team_runs SET payload=json_set(payload,'$.result','retained result','$.extension',json('{"reason":"retained"}')) WHERE run_id=?`, tc.id)
			must(t, err)
			_, err = db.DB().Exec(`INSERT INTO team_run_deadlines(run_id,deadline,recorded_at) VALUES(?,'2099-01-01T00:00:00Z','now')`, tc.id)
			must(t, err)
			// Use a new store/connection as on daemon restart.
			_, restarted := open(t, path, teamstore.Options{})
			for i := 0; i < 2; i++ {
				must(t, restarted.ReconcileDissolvedRuns(ctx, 50))
				got, err := restarted.GetRun(ctx, tc.id)
				must(t, err)
				want := "working"
				if tc.want {
					want = "dissolved"
				}
				if string(got.Run.Status) != want {
					t.Fatalf("%s: status=%s", tc.id, got.Run.Status)
				}
				var payload []byte
				must(t, db.DB().QueryRow(`SELECT payload FROM team_runs WHERE run_id=?`, tc.id).Scan(&payload))
				var fields map[string]any
				must(t, json.Unmarshal(payload, &fields))
				if fields["result"] != "retained result" || fields["extension"].(map[string]any)["reason"] != "retained" {
					t.Fatalf("metadata lost: %s", payload)
				}
				var closed bool
				must(t, db.DB().QueryRow(`SELECT closed FROM team_run_deadlines WHERE run_id=?`, tc.id).Scan(&closed))
				if closed != tc.want {
					t.Fatalf("closed=%v", closed)
				}
			}
		})
	}
	if _, err := s.ReconcileDissolvedRun(ctx, "missing"); !errors.Is(err, teams.ErrNotFound) {
		t.Fatalf("missing=%v", err)
	}
	// Existing terminal outcomes and unknown extension fields survive both paths.
	_, err := db.DB().Exec(`UPDATE team_runs SET payload=json_set(payload,'$.status','failed') WHERE run_id='ended'`)
	must(t, err)
	must(t, s.MarkRunDissolved(ctx, "ended"))
	settled, err := s.ReconcileDissolvedRun(ctx, "ended")
	must(t, err)
	got, err := s.GetRun(ctx, "ended")
	must(t, err)
	if !settled || got.Run.Status != "failed" {
		t.Fatal("terminal outcome overwritten")
	}
}
